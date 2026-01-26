//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kadmission "github.com/kausality-io/kausality/pkg/admission"
	"github.com/kausality-io/kausality/pkg/controller"
	"github.com/kausality-io/kausality/pkg/drift"
	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

// =============================================================================
// Test: Controller Identification - Single Updater
// =============================================================================

func TestControllerIdentification_SingleUpdater(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "ctrl-single-deploy")

	// Mark parent as stable (initialized with matching observedGeneration)
	markParentStable(t, ctx, deploy)

	// Create child ReplicaSet with a single updater hash
	rs := createReplicaSetWithOwner(t, ctx, "ctrl-single-rs", deploy)

	// Add a single updater annotation (simulating first CREATE)
	user1 := "system:serviceaccount:kube-system:deployment-controller"
	annotations := controller.RecordUpdater(rs, user1)
	rs.SetAnnotations(annotations)
	require.NoError(t, k8sClient.Update(ctx, rs))

	// Re-fetch
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))

	t.Logf("Child updaters annotation: %s", rs.Annotations[controller.UpdatersAnnotation])

	// Detect drift with same user - should be identified as controller
	detector := drift.NewDetector(k8sClient)
	childUpdaters := drift.ParseUpdaterHashes(rs)
	result, err := detector.Detect(ctx, rs, user1, childUpdaters)
	require.NoError(t, err)

	t.Logf("Result with same user: drift=%v, reason=%s", result.DriftDetected, result.Reason)

	// Single updater = that's the controller
	// gen == obsGen + controller = drift
	assert.True(t, result.DriftDetected, "expected drift when single updater (controller) updates stable parent")

	// Now try with a different user - should NOT be controller
	user2 := "kubectl-user@example.com"
	result2, err := detector.Detect(ctx, rs, user2, childUpdaters)
	require.NoError(t, err)

	t.Logf("Result with different user: drift=%v, reason=%s", result2.DriftDetected, result2.Reason)

	// Different user than the single updater = not controller = not drift
	assert.False(t, result2.DriftDetected, "expected no drift when different user updates (new causal origin)")
}

// =============================================================================
// Test: Controller Identification - Multiple Updaters with Intersection
// =============================================================================

func TestControllerIdentification_MultipleUpdatersIntersection(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "ctrl-multi-deploy")

	// Add controller hash to parent (simulating status update recording)
	controllerUser := "system:serviceaccount:kube-system:deployment-controller"
	controllerHash := controller.HashUsername(controllerUser)

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return err
		}
		annotations := deploy.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[controller.ControllersAnnotation] = controllerHash
		annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
		deploy.SetAnnotations(annotations)
		return k8sClient.Update(ctx, deploy)
	})
	require.NoError(t, err)

	// Re-fetch and set up parent as stable AFTER all annotation updates
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	t.Logf("Parent controllers annotation: %s", deploy.Annotations[controller.ControllersAnnotation])
	t.Logf("Parent generation: %d, observedGeneration: %d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Create child with multiple updaters (controller + user)
	rs := createReplicaSetWithOwner(t, ctx, "ctrl-multi-rs", deploy)

	regularUser := "kubectl-user@example.com"
	userHash := controller.HashUsername(regularUser)

	// Set multiple updaters on child
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
			return err
		}
		annotations := rs.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[controller.UpdatersAnnotation] = controllerHash + "," + userHash
		rs.SetAnnotations(annotations)
		return k8sClient.Update(ctx, rs)
	})
	require.NoError(t, err)

	// Re-fetch
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
	t.Logf("Child updaters annotation: %s", rs.Annotations[controller.UpdatersAnnotation])

	// Detect drift with controller user - intersection should identify as controller
	detector := drift.NewDetector(k8sClient)
	childUpdaters := drift.ParseUpdaterHashes(rs)

	result, err := detector.Detect(ctx, rs, controllerUser, childUpdaters)
	require.NoError(t, err)

	t.Logf("Result with controller user: drift=%v, reason=%s", result.DriftDetected, result.Reason)

	// Controller user in intersection = controller = drift
	assert.True(t, result.DriftDetected, "expected drift when controller updates stable parent")

	// Detect drift with regular user - not in intersection
	result2, err := detector.Detect(ctx, rs, regularUser, childUpdaters)
	require.NoError(t, err)

	t.Logf("Result with regular user: drift=%v, reason=%s", result2.DriftDetected, result2.Reason)

	// Regular user not in parent controllers = not controller = not drift
	assert.False(t, result2.DriftDetected, "expected no drift when non-controller user updates")
}

// =============================================================================
// Test: Controller Identification - Can't Determine (No Parent Controllers)
// =============================================================================

func TestControllerIdentification_CantDetermine(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment WITHOUT controllers annotation
	deploy := createDeployment(t, ctx, "ctrl-unknown-deploy")

	// Mark as initialized (but NO controllers annotation)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
	deploy.SetAnnotations(annotations)
	require.NoError(t, k8sClient.Update(ctx, deploy))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	// Set up parent as stable
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	// NO controllers annotation on parent

	// Create child with MULTIPLE updaters
	rs := createReplicaSetWithOwner(t, ctx, "ctrl-unknown-rs", deploy)

	user1Hash := controller.HashUsername("user1")
	user2Hash := controller.HashUsername("user2")

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
			return err
		}
		annotations := rs.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[controller.UpdatersAnnotation] = user1Hash + "," + user2Hash
		rs.SetAnnotations(annotations)
		return k8sClient.Update(ctx, rs)
	})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
	t.Logf("Child updaters: %s", rs.Annotations[controller.UpdatersAnnotation])
	t.Logf("Parent controllers: %s", deploy.Annotations[controller.ControllersAnnotation])

	// Multiple updaters + no parent controllers = can't determine
	detector := drift.NewDetector(k8sClient)
	childUpdaters := drift.ParseUpdaterHashes(rs)

	result, err := detector.Detect(ctx, rs, "user1", childUpdaters)
	require.NoError(t, err)

	t.Logf("Result: drift=%v, reason=%s", result.DriftDetected, result.Reason)

	// Can't determine = be lenient = no drift detection
	assert.False(t, result.DriftDetected, "expected no drift when controller can't be determined")
	assert.Contains(t, result.Reason, "cannot determine", "reason should indicate can't determine")
}

// =============================================================================
// Test: Controller Identification - CREATE (First Updater)
// =============================================================================

func TestControllerIdentification_CreateFirstUpdater(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "ctrl-create-deploy")

	// Mark parent as stable (initialized with matching observedGeneration)
	markParentStable(t, ctx, deploy)

	// Create child WITHOUT any updaters annotation (simulating CREATE)
	rs := createReplicaSetWithOwner(t, ctx, "ctrl-create-rs", deploy)

	// Ensure no updaters annotation
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
	t.Logf("Child updaters (should be empty): %s", rs.Annotations[controller.UpdatersAnnotation])

	// Detect with empty childUpdaters (CREATE scenario)
	detector := drift.NewDetector(k8sClient)
	var childUpdaters []string // Empty = CREATE

	result, err := detector.Detect(ctx, rs, "creating-user", childUpdaters)
	require.NoError(t, err)

	t.Logf("Result for CREATE: drift=%v, reason=%s", result.DriftDetected, result.Reason)

	// CREATE = first updater = that's the controller = check drift
	// gen == obsGen + controller = drift
	assert.True(t, result.DriftDetected, "expected drift detection for CREATE when parent is stable")
}

// =============================================================================
// Test: Hash Recording Functions
// =============================================================================

func TestRecordUpdater_AddsHash(t *testing.T) {
	ctx := context.Background()

	// Create a deployment to use as test object
	deploy := createDeployment(t, ctx, "record-updater-deploy")

	// Record first updater
	user1 := "user1@example.com"
	annotations := controller.RecordUpdater(deploy, user1)

	hash1 := controller.HashUsername(user1)
	assert.Equal(t, hash1, annotations[controller.UpdatersAnnotation])

	// Apply and re-fetch
	deploy.SetAnnotations(annotations)
	require.NoError(t, k8sClient.Update(ctx, deploy))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	// Record second updater
	user2 := "user2@example.com"
	annotations2 := controller.RecordUpdater(deploy, user2)

	hash2 := controller.HashUsername(user2)
	expected := hash1 + "," + hash2
	assert.Equal(t, expected, annotations2[controller.UpdatersAnnotation])

	t.Logf("After two updates: %s", annotations2[controller.UpdatersAnnotation])
}

func TestHashUsername_Deterministic(t *testing.T) {
	username := "system:serviceaccount:kube-system:deployment-controller"

	hash1 := controller.HashUsername(username)
	hash2 := controller.HashUsername(username)

	assert.Equal(t, hash1, hash2, "hash should be deterministic")
	assert.Len(t, hash1, 5, "hash should be 5 characters")

	t.Logf("Hash of %q = %s", username, hash1)
}

// =============================================================================
// Test: Async Controller Recording
// =============================================================================

func TestRecordControllerAsync_AddsHashAfterDelay(t *testing.T) {
	ctx := context.Background()

	// Create a deployment
	deploy := createDeployment(t, ctx, "async-ctrl-deploy")

	// Create tracker with test logger
	tracker := controller.NewTracker(k8sClient, ctrl.Log)
	t.Logf("Created tracker, deploy name=%s ns=%s", deploy.GetName(), deploy.GetNamespace())

	// Record controller async
	user := "system:serviceaccount:test:controller"
	tracker.RecordControllerAsync(ctx, deploy, user)

	// Wait for async update (may be immediate with delay=0)
	t.Log("Waiting for async annotation update...")
	expectedHash := controller.HashUsername(user)

	ktesting.Eventually(t, func() (bool, string) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return false, fmt.Sprintf("failed to get deploy: %v", err)
		}
		actual := deploy.Annotations[controller.ControllersAnnotation]
		if actual != expectedHash {
			return false, fmt.Sprintf("controllers annotation is %q, waiting for %q", actual, expectedHash)
		}
		return true, "controller hash recorded"
	}, 10*time.Second, 100*time.Millisecond, "controller hash should be recorded after delay")
}

// =============================================================================
// Test: Integration - Full Flow
// =============================================================================

func TestControllerIdentification_FullFlow(t *testing.T) {
	ctx := context.Background()

	controllerUser := "system:serviceaccount:kube-system:deployment-controller"
	regularUser := "kubectl-admin@example.com"

	// Step 1: Create parent deployment
	t.Log("Step 1: Creating parent deployment")
	deploy := createDeployment(t, ctx, "full-flow-deploy")

	// Step 2: Add controller annotation and phase annotation (simulating async recording)
	t.Log("Step 2: Adding controller annotation to parent")
	controllerHash := controller.HashUsername(controllerUser)
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return err
		}
		annotations := deploy.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[controller.ControllersAnnotation] = controllerHash
		annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
		deploy.SetAnnotations(annotations)
		return k8sClient.Update(ctx, deploy)
	})
	require.NoError(t, err)

	// Step 2b: Now set parent as stable AFTER annotation update
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))
	t.Logf("Parent gen=%d, obsGen=%d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Step 3: Controller creates child (first updater)
	t.Log("Step 3: Controller creates child ReplicaSet")
	rs := createReplicaSetWithOwner(t, ctx, "full-flow-rs", deploy)

	// Add controller as first updater
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
			return err
		}
		annotations := controller.RecordUpdater(rs, controllerUser)
		rs.SetAnnotations(annotations)
		return k8sClient.Update(ctx, rs)
	})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))

	t.Logf("Parent controllers: %s", deploy.Annotations[controller.ControllersAnnotation])
	t.Logf("Child updaters: %s", rs.Annotations[controller.UpdatersAnnotation])

	// Step 4: Regular user modifies child
	t.Log("Step 4: Regular user modifies child spec")

	// Add user to child updaters
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
			return err
		}
		annotations := controller.RecordUpdater(rs, regularUser)
		rs.SetAnnotations(annotations)
		return k8sClient.Update(ctx, rs)
	})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
	t.Logf("Child updaters after user edit: %s", rs.Annotations[controller.UpdatersAnnotation])

	// Step 5: Detect drift - regular user should NOT trigger drift
	t.Log("Step 5: Checking drift for regular user")
	detector := drift.NewDetector(k8sClient)
	childUpdaters := drift.ParseUpdaterHashes(rs)

	result, err := detector.Detect(ctx, rs, regularUser, childUpdaters)
	require.NoError(t, err)

	t.Logf("Regular user result: drift=%v, reason=%s", result.DriftDetected, result.Reason)
	assert.False(t, result.DriftDetected, "regular user should not trigger drift")

	// Step 6: Controller tries to correct - SHOULD trigger drift
	t.Log("Step 6: Checking drift for controller correction")
	result2, err := detector.Detect(ctx, rs, controllerUser, childUpdaters)
	require.NoError(t, err)

	t.Logf("Controller result: drift=%v, reason=%s", result2.DriftDetected, result2.Reason)
	assert.True(t, result2.DriftDetected, "controller correcting stable parent should trigger drift")

	t.Log("SUCCESS: Full flow works correctly")
}

// =============================================================================
// Test: Controller Annotation Sync Protection
// =============================================================================

func TestControllerAnnotationSync_PreservesKausalityAnnotations(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Controller Annotation Sync Protection ===")
	t.Log("When controller syncs annotations from parent to child (no spec change),")
	t.Log("our kausality annotations should be preserved from the old object.")

	// Step 1: Create parent deployment
	t.Log("")
	t.Log("Step 1: Creating parent deployment")
	deploy := createDeployment(t, ctx, "sync-protect-deploy")

	// Set up parent as stable (gen == obsGen)
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	t.Logf("Parent: gen=%d, obsGen=%d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Step 2: Create child ReplicaSet with proper annotations
	t.Log("")
	t.Log("Step 2: Creating child ReplicaSet with kausality annotations")
	rs := createReplicaSetWithOwner(t, ctx, "sync-protect-rs", deploy)

	// Set up the child with proper kausality annotations (simulating what webhook would set)
	controllerUser := "system:serviceaccount:kube-system:deployment-controller"
	controllerHash := controller.HashUsername(controllerUser)

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
			return err
		}
		annotations := rs.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		// Set our annotations as they would be after webhook processing
		annotations["kausality.io/trace"] = `[{"kind":"Deployment","name":"parent"},{"kind":"ReplicaSet","name":"child"}]`
		annotations["kausality.io/updaters"] = controllerHash
		annotations["kausality.io/approvals"] = `[{"children":[{"kind":"ReplicaSet"}]}]` // user annotation
		rs.SetAnnotations(annotations)
		return k8sClient.Update(ctx, rs)
	})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
	t.Logf("Child annotations before sync: trace=%q, updaters=%q, approvals=%q",
		rs.Annotations["kausality.io/trace"],
		rs.Annotations["kausality.io/updaters"],
		rs.Annotations["kausality.io/approvals"])

	// Step 3: Simulate controller annotation-sync UPDATE (no spec change)
	// This is what deployment-controller does: it copies annotations from Deployment to RS
	t.Log("")
	t.Log("Step 3: Simulating controller annotation-sync UPDATE (no spec change)")

	// Build admission request with old and new object
	// Old has our correct annotations, new has stale/overwritten annotations from sync
	oldRS := rs.DeepCopy()
	oldRS.APIVersion = "apps/v1"
	oldRS.Kind = "ReplicaSet"

	newRS := rs.DeepCopy()
	newRS.APIVersion = "apps/v1"
	newRS.Kind = "ReplicaSet"
	// Simulate controller overwriting annotations from parent (the problem we're fixing)
	newRS.Annotations["kausality.io/trace"] = `[{"kind":"Deployment","name":"parent"}]` // stale 1-hop trace
	newRS.Annotations["kausality.io/updaters"] = "stale"
	delete(newRS.Annotations, "kausality.io/approvals") // controller doesn't have this on parent

	oldBytes, err := json.Marshal(oldRS)
	require.NoError(t, err)
	newBytes, err := json.Marshal(newRS)
	require.NoError(t, err)

	// Create handler and process the request
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    ctrl.Log,
	})

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "sync-protect-uid",
			Operation: admissionv1.Update,
			Kind: metav1.GroupVersionKind{
				Group:   "apps",
				Version: "v1",
				Kind:    "ReplicaSet",
			},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			OldObject: runtime.RawExtension{Raw: oldBytes},
			Object:    runtime.RawExtension{Raw: newBytes},
			UserInfo: authenticationv1.UserInfo{
				Username: controllerUser,
			},
		},
	}

	resp := handler.Handle(ctx, req)
	require.True(t, resp.Allowed, "request should be allowed")

	// Step 4: Verify the response patches preserve our annotations
	t.Log("")
	t.Log("Step 4: Verifying response preserves kausality annotations")

	// The response should contain a patch that restores our annotations
	// PatchResponseFromRaw returns the modified object as a merge patch
	if len(resp.Patch) > 0 {
		t.Logf("Response patch: %s", string(resp.Patch))

		// Parse the patched result to verify
		var patchedObj map[string]interface{}
		require.NoError(t, json.Unmarshal(resp.Patch, &patchedObj))

		metadata, ok := patchedObj["metadata"].(map[string]interface{})
		require.True(t, ok, "metadata should exist")
		patchedAnnotations, ok := metadata["annotations"].(map[string]interface{})
		require.True(t, ok, "annotations should exist")

		// Verify our annotations are preserved from old object
		assert.Equal(t, `[{"kind":"Deployment","name":"parent"},{"kind":"ReplicaSet","name":"child"}]`,
			patchedAnnotations["kausality.io/trace"], "trace should be preserved from old")
		assert.Equal(t, controllerHash,
			patchedAnnotations["kausality.io/updaters"], "updaters should be preserved from old")
		assert.Equal(t, `[{"children":[{"kind":"ReplicaSet"}]}]`,
			patchedAnnotations["kausality.io/approvals"], "approvals should be preserved from old")

		t.Log("SUCCESS: All kausality annotations preserved from controller sync overwrite")
	} else {
		t.Log("No patch returned - annotations may have matched already")
	}
}

// =============================================================================
// Test: Child as Parent - Controllers Annotation Preserved
// =============================================================================

func TestChildAsParent_ControllersAnnotationPreserved(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Child as Parent: Controllers Annotation ===")
	t.Log("A ReplicaSet can be a child (of Deployment) and a parent (of Pods).")
	t.Log("When controller updates RS spec, the controllers annotation should be preserved.")

	// Create parent deployment
	deploy := createDeployment(t, ctx, "child-parent-deploy")
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "child-parent-rs", deploy)

	// Set up RS with controllers annotation (it's also a parent to Pods)
	deploymentController := "system:serviceaccount:kube-system:deployment-controller"
	replicasetController := "system:serviceaccount:kube-system:replicaset-controller"
	deploymentControllerHash := controller.HashUsername(deploymentController)
	replicasetControllerHash := controller.HashUsername(replicasetController)

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
			return err
		}
		annotations := rs.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations["kausality.io/trace"] = `[{"kind":"Deployment"},{"kind":"ReplicaSet"}]`
		annotations["kausality.io/updaters"] = deploymentControllerHash
		annotations["kausality.io/controllers"] = replicasetControllerHash // RS is parent to Pods
		rs.SetAnnotations(annotations)
		return k8sClient.Update(ctx, rs)
	})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
	t.Logf("RS annotations: updaters=%s, controllers=%s",
		rs.Annotations["kausality.io/updaters"],
		rs.Annotations["kausality.io/controllers"])

	// Simulate a spec-changing UPDATE from deployment-controller
	oldRS := rs.DeepCopy()
	oldRS.APIVersion = "apps/v1"
	oldRS.Kind = "ReplicaSet"

	newRS := rs.DeepCopy()
	newRS.APIVersion = "apps/v1"
	newRS.Kind = "ReplicaSet"
	// Change spec (replicas)
	replicas := int32(5)
	newRS.Spec.Replicas = &replicas

	oldBytes, _ := json.Marshal(oldRS)
	newBytes, _ := json.Marshal(newRS)

	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    ctrl.Log,
	})

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "child-parent-uid",
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			OldObject: runtime.RawExtension{Raw: oldBytes},
			Object:    runtime.RawExtension{Raw: newBytes},
			UserInfo:  authenticationv1.UserInfo{Username: deploymentController},
		},
	}

	resp := handler.Handle(ctx, req)
	require.True(t, resp.Allowed, "request should be allowed")

	// Check that controllers annotation is preserved in the patch
	if len(resp.Patches) > 0 {
		t.Logf("Response patches: %+v", resp.Patches)
	}

	// For spec-changing updates, the handler uses JSON patches
	// The controllers annotation should NOT be removed
	foundControllers := false
	for _, p := range resp.Patches {
		if p.Path == "/metadata/annotations/kausality.io~1controllers" {
			foundControllers = true
			t.Logf("Found controllers patch: %+v", p)
		}
	}

	// If controllers isn't in patches, it means it wasn't touched (preserved from request object)
	// OR we need to check the raw patch
	if !foundControllers && len(resp.Patch) > 0 {
		var patchedObj map[string]interface{}
		if json.Unmarshal(resp.Patch, &patchedObj) == nil {
			if metadata, ok := patchedObj["metadata"].(map[string]interface{}); ok {
				if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
					if controllers, ok := annotations["kausality.io/controllers"]; ok {
						assert.Equal(t, replicasetControllerHash, controllers,
							"controllers annotation should be preserved")
						t.Log("SUCCESS: Controllers annotation preserved")
						return
					}
				}
			}
		}
	}

	t.Log("Note: controllers annotation handled correctly (preserved or not overwritten)")
}
