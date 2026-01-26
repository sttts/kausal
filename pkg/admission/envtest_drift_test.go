//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kausality-io/kausality/pkg/controller"
	"github.com/kausality-io/kausality/pkg/drift"
)

func TestControllerIdentification_ManagedFields(t *testing.T) {
	ctx := context.Background()

	// Create a Deployment
	deploy := createDeploymentUnit(t, ctx, "ctrl-id-deploy")

	// Simulate controller updating status (sets observedGeneration)
	// In real scenario, the deployment controller does this
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = 1
	deploy.Status.ReadyReplicas = 1
	if err := k8sClientUnit.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment status: %v", err)
	}

	// Re-fetch to get managedFields
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("Deployment managedFields: %d entries", len(deploy.ManagedFields))
	for _, mf := range deploy.ManagedFields {
		t.Logf("  Manager: %s, Operation: %s, Subresource: %s", mf.Manager, mf.Operation, mf.Subresource)
	}

	// Verify we can find the controller manager
	// The status update should have created a managedFields entry
	resolver := drift.NewParentResolver(k8sClientUnit)

	// Create a child ReplicaSet with ownerRef
	rs := createReplicaSetWithOwnerUnit(t, ctx, "ctrl-id-rs", deploy)

	// Resolve parent and check controller manager is populated
	parentState, err := resolver.ResolveParent(ctx, rs)
	if err != nil {
		t.Fatalf("failed to resolve parent: %v", err)
	}

	if parentState == nil {
		t.Fatal("expected parent state, got nil")
	}

	t.Logf("Parent state: gen=%d, obsGen=%d, controllers=%v",
		parentState.Generation, parentState.ObservedGeneration, parentState.Controllers)
}

// =============================================================================
// Test: Drift Detection - Expected Change (gen != obsGen)
// =============================================================================

func TestDriftDetection_ExpectedChange(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeploymentUnit(t, ctx, "expected-change-deploy")

	// Don't update status yet - generation != observedGeneration (0)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerUnit(t, ctx, "expected-change-rs", deploy)

	// Detect drift - use test-user with nil childUpdaters (CREATE case, assumes controller)
	detector := drift.NewDetector(k8sClientUnit)
	result, err := detector.Detect(ctx, rs, "test-user", nil)
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result: allowed=%v, drift=%v, phase=%v, reason=%s",
		result.Allowed, result.DriftDetected, result.LifecyclePhase, result.Reason)

	// Parent is initializing (no observedGeneration) - should allow
	if !result.Allowed {
		t.Errorf("expected allowed=true, got false")
	}

	// Now set observedGeneration < generation (reconciling)
	deploy.Status.ObservedGeneration = deploy.Generation - 1
	if err := k8sClientUnit.Status().Update(ctx, deploy); err != nil {
		// If generation is 1, we can't set obsGen to 0 and have it < gen
		// Just update to same value and then bump generation
		deploy.Status.ObservedGeneration = deploy.Generation
		_ = k8sClientUnit.Status().Update(ctx, deploy)

		// Bump generation by updating spec
		replicas := int32(2)
		deploy.Spec.Replicas = &replicas
		if err := k8sClientUnit.Update(ctx, deploy); err != nil {
			t.Fatalf("failed to update deployment: %v", err)
		}
	}

	// Re-fetch
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("After update: gen=%d, obsGen=%d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Now gen != obsGen - should be expected change
	result, err = detector.Detect(ctx, rs, "test-user", nil)
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result: allowed=%v, drift=%v, phase=%v, reason=%s",
		result.Allowed, result.DriftDetected, result.LifecyclePhase, result.Reason)

	if !result.Allowed {
		t.Errorf("expected allowed=true for reconciling parent")
	}
	if result.DriftDetected {
		t.Errorf("expected driftDetected=false for reconciling parent")
	}
}

// =============================================================================
// Test: Drift Detection - Drift (gen == obsGen)
// =============================================================================

func TestDriftDetection_Drift(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeploymentUnit(t, ctx, "drift-deploy")

	// Mark parent as stable (initialized with matching observedGeneration)
	markParentStableUnit(t, ctx, deploy)

	t.Logf("Deployment: gen=%d, obsGen=%d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerUnit(t, ctx, "drift-rs", deploy)

	// Detect drift - use test-user with nil childUpdaters (CREATE case, assumes controller)
	detector := drift.NewDetector(k8sClientUnit)
	result, err := detector.Detect(ctx, rs, "test-user", nil)
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result: allowed=%v, drift=%v, phase=%v, reason=%s",
		result.Allowed, result.DriftDetected, result.LifecyclePhase, result.Reason)

	// gen == obsGen - drift should be detected
	if !result.DriftDetected {
		t.Errorf("expected driftDetected=true when gen == obsGen")
	}
	// Phase 1: always allow
	if !result.Allowed {
		t.Errorf("expected allowed=true (Phase 1 logging only)")
	}
}

// =============================================================================
// Test: Trace Propagation - New Origin
// =============================================================================

func TestLifecyclePhase_Initializing(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment (no observedGeneration yet = initializing)
	deploy := createDeploymentUnit(t, ctx, "lifecycle-init-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerUnit(t, ctx, "lifecycle-init-rs", deploy)

	// Detect drift - use test-user with nil childUpdaters (CREATE case, assumes controller)
	detector := drift.NewDetector(k8sClientUnit)
	result, err := detector.Detect(ctx, rs, "test-user", nil)
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result: phase=%v, allowed=%v, drift=%v", result.LifecyclePhase, result.Allowed, result.DriftDetected)

	if result.LifecyclePhase != drift.PhaseInitializing {
		t.Errorf("expected phase Initializing, got %v", result.LifecyclePhase)
	}

	if !result.Allowed {
		t.Errorf("expected allowed=true during initialization")
	}

	if result.DriftDetected {
		t.Errorf("expected driftDetected=false during initialization")
	}
}

// =============================================================================
// Test: Lifecycle Phase - Deleting
// =============================================================================

func TestLifecyclePhase_Deleting(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment with finalizer
	deploy := createDeploymentUnit(t, ctx, "lifecycle-delete-deploy")
	deploy.Finalizers = []string{"test.kausality.io/finalizer"}
	if err := k8sClientUnit.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to add finalizer: %v", err)
	}

	// Set observedGeneration (mark as ready)
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClientUnit.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerUnit(t, ctx, "lifecycle-delete-rs", deploy)

	// Delete the deployment (will be blocked by finalizer, but sets deletionTimestamp)
	if err := k8sClientUnit.Delete(ctx, deploy); err != nil {
		t.Fatalf("failed to delete deployment: %v", err)
	}

	// Re-fetch deployment to get deletionTimestamp
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	if deploy.DeletionTimestamp == nil {
		t.Fatal("expected deletionTimestamp to be set")
	}

	t.Logf("Deployment deletionTimestamp: %v", deploy.DeletionTimestamp)

	// Detect drift - use test-user with nil childUpdaters (CREATE case, assumes controller)
	detector := drift.NewDetector(k8sClientUnit)
	result, err := detector.Detect(ctx, rs, "test-user", nil)
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result: phase=%v, allowed=%v, drift=%v", result.LifecyclePhase, result.Allowed, result.DriftDetected)

	if result.LifecyclePhase != drift.PhaseDeleting {
		t.Errorf("expected phase Deleting, got %v", result.LifecyclePhase)
	}

	if !result.Allowed {
		t.Errorf("expected allowed=true during deletion")
	}

	// Clean up: remove finalizer
	deploy.Finalizers = nil
	if err := k8sClientUnit.Update(ctx, deploy); err != nil {
		t.Logf("failed to remove finalizer: %v", err)
	}
}

// =============================================================================
// Test: Admission Handler Integration
// =============================================================================

func TestNoOwnerReference_RootObject(t *testing.T) {
	ctx := context.Background()

	// Create a deployment without any owner (root object)
	deploy := createDeploymentUnit(t, ctx, "root-deploy")

	// Detect drift on root object - use test-user with nil childUpdaters
	detector := drift.NewDetector(k8sClientUnit)
	result, err := detector.Detect(ctx, deploy, "test-user", nil)
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result: allowed=%v, drift=%v, reason=%s", result.Allowed, result.DriftDetected, result.Reason)

	// No controller ownerRef - should allow, no drift
	if !result.Allowed {
		t.Errorf("expected allowed=true for root object")
	}
	if result.DriftDetected {
		t.Errorf("expected driftDetected=false for root object (no parent to drift from)")
	}
	if result.ParentRef != nil {
		t.Errorf("expected ParentRef=nil for root object, got %v", result.ParentRef)
	}
}

// =============================================================================
// Test: Lifecycle Phase - Ready Condition
// =============================================================================

func TestLifecyclePhase_ReadyCondition(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeploymentUnit(t, ctx, "ready-cond-deploy")

	// Set Ready=True condition (simulating a CRD with conditions but no observedGeneration yet)
	deploy.Status.Conditions = []appsv1.DeploymentCondition{
		{
			Type:   appsv1.DeploymentAvailable,
			Status: corev1.ConditionTrue,
		},
	}
	if err := k8sClientUnit.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerUnit(t, ctx, "ready-cond-rs", deploy)

	// Resolve parent state
	resolver := drift.NewParentResolver(k8sClientUnit)
	parentState, err := resolver.ResolveParent(ctx, rs)
	if err != nil {
		t.Fatalf("failed to resolve parent: %v", err)
	}

	// Check lifecycle phase - deployment conditions are different from metav1.Condition
	// The lifecycle detector looks for Ready/Initialized in metav1.Condition format
	// Deployments use appsv1.DeploymentCondition which is different
	// This test verifies the behavior with standard deployment conditions
	lifecycleDetector := drift.NewLifecycleDetector()
	phase := lifecycleDetector.DetectPhase(parentState)

	t.Logf("Parent state: hasObsGen=%v, conditions=%d, phase=%v",
		parentState.HasObservedGeneration, len(parentState.Conditions), phase)
}

// =============================================================================
// Test: Different Actor Creates New Trace Origin
// =============================================================================

func TestMultipleOwnerRefs_OnlyControllerMatters(t *testing.T) {
	ctx := context.Background()

	// Create two deployments - one will be controller, one won't
	controllerDeploy := createDeploymentUnit(t, ctx, "multi-owner-ctrl")
	nonControllerDeploy := createDeploymentUnit(t, ctx, "multi-owner-nonctrl")

	// Mark controller as initialized first
	annotations := controllerDeploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
	controllerDeploy.SetAnnotations(annotations)
	if err := k8sClientUnit.Update(ctx, controllerDeploy); err != nil {
		t.Fatalf("failed to update controller annotations: %v", err)
	}
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(controllerDeploy), controllerDeploy); err != nil {
		t.Fatalf("failed to get controller deploy: %v", err)
	}

	// Set controller as stable (drift scenario)
	controllerDeploy.Status.ObservedGeneration = controllerDeploy.Generation
	if err := k8sClientUnit.Status().Update(ctx, controllerDeploy); err != nil {
		t.Fatalf("failed to update controller status: %v", err)
	}

	// Set non-controller as reconciling (would be "expected" if it were the controller)
	nonControllerDeploy.Status.ObservedGeneration = nonControllerDeploy.Generation - 1
	// This will fail if generation is 1, so just set it to same
	if nonControllerDeploy.Generation == 1 {
		nonControllerDeploy.Status.ObservedGeneration = 0
	}
	_ = k8sClientUnit.Status().Update(ctx, nonControllerDeploy) // Ignore error

	// Re-fetch
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(controllerDeploy), controllerDeploy); err != nil {
		t.Fatalf("failed to get controller deploy: %v", err)
	}

	// Create ReplicaSet with BOTH owners, but only one is controller
	trueVal := true
	falseVal := false
	testCounter++
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("multi-owner-rs-%d", testCounter),
			Namespace: testNSUnit,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       nonControllerDeploy.Name,
					UID:        nonControllerDeploy.UID,
					Controller: &falseVal, // NOT the controller
				},
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       controllerDeploy.Name,
					UID:        controllerDeploy.UID,
					Controller: &trueVal, // THIS is the controller
				},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: func() *int32 { v := int32(1); return &v }(),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "multi-owner"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "multi-owner"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClientUnit.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	// Detect drift - should use controllerDeploy (the one with controller: true)
	// Use test-user with nil childUpdaters (CREATE case, assumes controller)
	detector := drift.NewDetector(k8sClientUnit)
	result, err := detector.Detect(ctx, rs, "test-user", nil)
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result: allowed=%v, drift=%v, parentRef=%v, reason=%s",
		result.Allowed, result.DriftDetected, result.ParentRef, result.Reason)

	// Should detect drift because controllerDeploy has gen == obsGen
	if !result.DriftDetected {
		t.Errorf("expected driftDetected=true (controller parent is stable)")
	}

	// ParentRef should point to the controller owner
	if result.ParentRef == nil {
		t.Fatal("expected ParentRef to be set")
	}
	if result.ParentRef.Name != controllerDeploy.Name {
		t.Errorf("expected ParentRef.Name=%q (controller), got %q",
			controllerDeploy.Name, result.ParentRef.Name)
	}
}

// =============================================================================
// Test: Lifecycle Phase - Phase Annotation Persistence
// =============================================================================

func TestLifecyclePhase_InitializedAnnotation(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeploymentUnit(t, ctx, "init-annot-deploy")

	// Set the kausality.io/phase annotation (simulating previous webhook setting it)
	deploy.SetAnnotations(map[string]string{
		controller.PhaseAnnotation: controller.PhaseValueInitialized,
	})
	if err := k8sClientUnit.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

	// Re-fetch
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerUnit(t, ctx, "init-annot-rs", deploy)

	// Resolve parent state - should detect IsInitialized from annotation
	resolver := drift.NewParentResolver(k8sClientUnit)
	parentState, err := resolver.ResolveParent(ctx, rs)
	if err != nil {
		t.Fatalf("failed to resolve parent: %v", err)
	}

	if !parentState.IsInitialized {
		t.Errorf("expected IsInitialized=true from annotation, got false")
	}

	// Detect lifecycle phase - should be Initialized due to annotation
	lifecycleDetector := drift.NewLifecycleDetector()
	phase := lifecycleDetector.DetectPhase(parentState)

	t.Logf("Parent state: IsInitialized=%v, hasObsGen=%v, phase=%v",
		parentState.IsInitialized, parentState.HasObservedGeneration, phase)

	if phase != drift.PhaseInitialized {
		t.Errorf("expected PhaseInitialized from annotation, got %v", phase)
	}
}

// =============================================================================
// Test: Metadata-only changes allowed during drift protection
// =============================================================================

func TestMetadataOnlyChanges_AllowedDuringDriftProtection(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeploymentUnit(t, ctx, "metadata-only-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerUnit(t, ctx, "metadata-only-rs", deploy)

	// Set parent as ready (drift scenario: gen == obsGen)
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClientUnit.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch RS
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}

	// Simulate a metadata-only change (adding a label)
	// This should NOT trigger drift detection
	labels := rs.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels["test-label"] = "test-value"
	rs.SetLabels(labels)

	// Update - should succeed without drift detection
	if err := k8sClientUnit.Update(ctx, rs); err != nil {
		t.Fatalf("metadata-only update should succeed: %v", err)
	}

	// Re-fetch and verify label was applied
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}

	if rs.Labels["test-label"] != "test-value" {
		t.Errorf("expected label to be applied, got: %v", rs.Labels)
	}

	t.Log("SUCCESS: Metadata-only changes are allowed during drift protection")
}

func TestAnnotationOnlyChanges_AllowedDuringDriftProtection(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeploymentUnit(t, ctx, "annot-only-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerUnit(t, ctx, "annot-only-rs", deploy)

	// Set parent as ready (drift scenario: gen == obsGen)
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClientUnit.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch RS
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}

	// Simulate an annotation-only change
	// This should NOT trigger drift detection
	annotations := rs.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["test-annotation"] = "test-value"
	rs.SetAnnotations(annotations)

	// Update - should succeed without drift detection
	if err := k8sClientUnit.Update(ctx, rs); err != nil {
		t.Fatalf("annotation-only update should succeed: %v", err)
	}

	// Re-fetch and verify annotation was applied
	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}

	if rs.Annotations["test-annotation"] != "test-value" {
		t.Errorf("expected annotation to be applied, got: %v", rs.Annotations)
	}

	t.Log("SUCCESS: Annotation-only changes are allowed during drift protection")
}

// =============================================================================
// Test: Approval - Mode Always
// =============================================================================
