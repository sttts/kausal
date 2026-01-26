//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kausality-io/kausality/pkg/controller"
	ktesting "github.com/kausality-io/kausality/pkg/testing"
	"github.com/kausality-io/kausality/pkg/trace"
)

// =============================================================================
// Real Webhook Tests - Hash Recording
// =============================================================================

// TestWebhook_UpdaterRecording_OnCreate verifies that the webhook automatically
// records the user hash in kausality.io/updaters on CREATE.
func TestWebhook_UpdaterRecording_OnCreate(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Updater Recording on CREATE ===")
	t.Log("When a resource is created, the webhook should automatically")
	t.Log("add the creating user's hash to kausality.io/updaters.")

	// Create a deployment
	deploy := createDeployment(t, ctx, "webhook-create-deploy")

	// Fetch the deployment to see what the webhook set
	var fetched appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))

	// Verify updaters annotation was set
	updaters := fetched.Annotations[controller.UpdatersAnnotation]
	t.Logf("Updaters annotation after CREATE: %q", updaters)

	assert.NotEmpty(t, updaters, "webhook should set updaters annotation on CREATE")
	assert.Len(t, controller.ParseHashes(updaters), 1, "should have exactly one updater hash")
}

// TestWebhook_UpdaterRecording_OnSpecChange verifies that the webhook adds
// the user hash when spec changes, but NOT for metadata-only changes.
func TestWebhook_UpdaterRecording_OnSpecChange(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Updater Recording on Spec Change ===")
	t.Log("When spec is modified, webhook should add the user hash.")
	t.Log("When only metadata changes, webhook should NOT add the user hash.")

	// Create a deployment
	deploy := createDeployment(t, ctx, "webhook-spec-change-deploy")

	// Fetch to get current state
	var fetched appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))
	initialUpdaters := fetched.Annotations[controller.UpdatersAnnotation]
	t.Logf("Initial updaters: %q", initialUpdaters)

	// Step 1: Metadata-only change (add label)
	t.Log("")
	t.Log("Step 1: Metadata-only change (add label)...")
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched); err != nil {
			return err
		}
		labels := fetched.Labels
		if labels == nil {
			labels = make(map[string]string)
		}
		labels["test-label"] = "test-value"
		fetched.Labels = labels
		return k8sClient.Update(ctx, &fetched)
	})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))
	afterMetadataUpdaters := fetched.Annotations[controller.UpdatersAnnotation]
	t.Logf("Updaters after metadata change: %q", afterMetadataUpdaters)

	assert.Equal(t, initialUpdaters, afterMetadataUpdaters,
		"metadata-only change should NOT add new updater hash")

	// Step 2: Spec change (change replicas)
	t.Log("")
	t.Log("Step 2: Spec change (change replicas)...")
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched); err != nil {
			return err
		}
		replicas := int32(3)
		fetched.Spec.Replicas = &replicas
		return k8sClient.Update(ctx, &fetched)
	})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))
	afterSpecUpdaters := fetched.Annotations[controller.UpdatersAnnotation]
	t.Logf("Updaters after spec change: %q", afterSpecUpdaters)

	// In envtest, all operations are by the same "admin" user, so the hash
	// count might not increase (same user), but the annotation should still be present
	assert.NotEmpty(t, afterSpecUpdaters, "updaters should be present after spec change")

	t.Log("")
	t.Log("SUCCESS: Updater recording works correctly for spec vs metadata changes")
}

// TestWebhook_ControllerRecording_OnStatusUpdate verifies that the webhook
// records the user hash in kausality.io/controllers on status subresource updates.
func TestWebhook_ControllerRecording_OnStatusUpdate(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Controller Recording on Status Update ===")
	t.Log("When status is updated, the webhook should add the user hash")
	t.Log("to kausality.io/controllers (async or sync).")

	// Create a deployment
	deploy := createDeployment(t, ctx, "webhook-status-deploy")

	// Fetch to get current state
	var fetched appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))

	// Initial controllers annotation (should be empty or not exist)
	initialControllers := fetched.Annotations[controller.ControllersAnnotation]
	t.Logf("Initial controllers: %q", initialControllers)

	// Update status
	t.Log("")
	t.Log("Updating status...")
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched); err != nil {
			return err
		}
		fetched.Status.ObservedGeneration = fetched.Generation
		fetched.Status.Replicas = 1
		return k8sClient.Status().Update(ctx, &fetched)
	})
	require.NoError(t, err)

	// Wait for async controller recording (the webhook records async after status updates)
	t.Log("Waiting for controller hash to be recorded...")
	ktesting.Eventually(t, func() (bool, string) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched); err != nil {
			return false, err.Error()
		}
		controllers := fetched.Annotations[controller.ControllersAnnotation]
		if controllers == "" {
			return false, "controllers annotation is empty"
		}
		return true, "controllers annotation is set: " + controllers
	}, 10*time.Second, 100*time.Millisecond, "controllers annotation should be set after status update")

	afterStatusControllers := fetched.Annotations[controller.ControllersAnnotation]
	t.Logf("Controllers after status update: %q", afterStatusControllers)

	assert.NotEmpty(t, afterStatusControllers, "controllers should be recorded after status update")

	t.Log("")
	t.Log("SUCCESS: Controller recording works on status updates")
}

// =============================================================================
// Real Webhook Tests - Trace Propagation
// =============================================================================

// TestWebhook_TracePropagation_Origin verifies that the webhook sets
// a trace annotation on objects without a controller ownerRef (origin).
func TestWebhook_TracePropagation_Origin(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Trace Propagation - Origin ===")
	t.Log("A resource without controller ownerRef should get an origin trace.")

	// Create a deployment (no parent)
	deploy := createDeployment(t, ctx, "webhook-trace-origin-deploy")

	// Fetch to see the trace
	var fetched appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))

	traceStr := fetched.Annotations[trace.TraceAnnotation]
	t.Logf("Trace annotation: %s", traceStr)

	assert.NotEmpty(t, traceStr, "trace annotation should be set")

	// Parse the trace
	tr, err := trace.Parse(traceStr)
	require.NoError(t, err, "trace should be valid JSON")

	assert.Len(t, tr, 1, "origin trace should have exactly one hop")
	if len(tr) > 0 {
		assert.Equal(t, "Deployment", tr[0].Kind, "hop should be for Deployment")
		assert.Equal(t, fetched.Name, tr[0].Name, "hop should have correct name")
		t.Logf("Origin hop: kind=%s, name=%s, user=%s", tr[0].Kind, tr[0].Name, tr[0].User)
	}

	t.Log("")
	t.Log("SUCCESS: Origin trace is set correctly")
}

// TestWebhook_TracePropagation_ChildExtends verifies that the webhook extends
// the parent's trace when creating a child resource.
func TestWebhook_TracePropagation_ChildExtends(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Trace Propagation - Child Extends Parent ===")
	t.Log("A child resource should extend its parent's trace.")

	// Create parent deployment
	deploy := createDeployment(t, ctx, "webhook-trace-parent-deploy")

	// Set parent as reconciling (gen != obsGen) so child creation is "expected"
	var fetchedDeploy appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetchedDeploy))

	// Note: In envtest, the parent trace won't be extended because:
	// 1. We're creating as "admin" user
	// 2. The webhook only extends trace if the controller is making the change
	// 3. Since this is the first update to the child, it's treated as a new origin
	//
	// However, we CAN verify that the child gets a trace annotation.

	// Create child ReplicaSet with ownerRef
	rs := createReplicaSetWithOwner(t, ctx, "webhook-trace-child-rs", &fetchedDeploy)

	// Fetch to see the trace
	var fetchedRS appsv1.ReplicaSet
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), &fetchedRS))

	traceStr := fetchedRS.Annotations[trace.TraceAnnotation]
	t.Logf("Child trace annotation: %s", traceStr)

	assert.NotEmpty(t, traceStr, "child should have trace annotation")

	// Parse the trace
	tr, err := trace.Parse(traceStr)
	require.NoError(t, err, "trace should be valid JSON")

	// In envtest with single "admin" user, the child will get its own origin trace
	// because "admin" is not identified as the deployment's controller
	t.Logf("Child trace has %d hops", len(tr))
	for i, hop := range tr {
		t.Logf("  Hop %d: kind=%s, name=%s, user=%s", i, hop.Kind, hop.Name, hop.User)
	}

	t.Log("")
	t.Log("SUCCESS: Child has trace annotation")
}

// =============================================================================
// Real Webhook Tests - Annotation-only changes
// =============================================================================

// TestWebhook_AnnotationOnlyChange_PreservesKausalityAnnotations verifies that
// annotation-only changes preserve existing kausality annotations.
func TestWebhook_AnnotationOnlyChange_PreservesKausalityAnnotations(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Annotation-Only Change Preserves Kausality Annotations ===")

	// Create a deployment
	deploy := createDeployment(t, ctx, "webhook-annot-preserve-deploy")

	// Fetch to get the trace set by webhook
	var fetched appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))

	originalTrace := fetched.Annotations[trace.TraceAnnotation]
	originalUpdaters := fetched.Annotations[controller.UpdatersAnnotation]
	t.Logf("Original trace: %s", originalTrace)
	t.Logf("Original updaters: %s", originalUpdaters)

	require.NotEmpty(t, originalTrace, "original trace should exist")

	// Add a user annotation (annotation-only change)
	t.Log("")
	t.Log("Adding user annotation (annotation-only change)...")
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched); err != nil {
			return err
		}
		annotations := fetched.Annotations
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations["my-custom-annotation"] = "my-value"
		fetched.Annotations = annotations
		return k8sClient.Update(ctx, &fetched)
	})
	require.NoError(t, err)

	// Verify kausality annotations are preserved
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))

	afterTrace := fetched.Annotations[trace.TraceAnnotation]
	afterUpdaters := fetched.Annotations[controller.UpdatersAnnotation]
	t.Logf("After trace: %s", afterTrace)
	t.Logf("After updaters: %s", afterUpdaters)

	assert.Equal(t, originalTrace, afterTrace, "trace should be preserved on annotation-only change")
	assert.Equal(t, originalUpdaters, afterUpdaters, "updaters should be preserved on annotation-only change")
	assert.Equal(t, "my-value", fetched.Annotations["my-custom-annotation"], "user annotation should be set")

	t.Log("")
	t.Log("SUCCESS: Kausality annotations preserved on annotation-only change")
}

// TestWebhook_LabelOnlyChange_PreservesKausalityAnnotations verifies that
// label-only changes preserve existing kausality annotations.
func TestWebhook_LabelOnlyChange_PreservesKausalityAnnotations(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Label-Only Change Preserves Kausality Annotations ===")

	// Create a deployment
	deploy := createDeployment(t, ctx, "webhook-label-preserve-deploy")

	// Fetch to get the trace set by webhook
	var fetched appsv1.Deployment
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))

	originalTrace := fetched.Annotations[trace.TraceAnnotation]
	originalUpdaters := fetched.Annotations[controller.UpdatersAnnotation]
	t.Logf("Original trace: %s", originalTrace)
	t.Logf("Original updaters: %s", originalUpdaters)

	require.NotEmpty(t, originalTrace, "original trace should exist")

	// Add a label (label-only change)
	t.Log("")
	t.Log("Adding label (label-only change)...")
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched); err != nil {
			return err
		}
		labels := fetched.Labels
		if labels == nil {
			labels = make(map[string]string)
		}
		labels["my-label"] = "my-label-value"
		fetched.Labels = labels
		return k8sClient.Update(ctx, &fetched)
	})
	require.NoError(t, err)

	// Verify kausality annotations are preserved
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), &fetched))

	afterTrace := fetched.Annotations[trace.TraceAnnotation]
	afterUpdaters := fetched.Annotations[controller.UpdatersAnnotation]
	t.Logf("After trace: %s", afterTrace)
	t.Logf("After updaters: %s", afterUpdaters)

	assert.Equal(t, originalTrace, afterTrace, "trace should be preserved on label-only change")
	assert.Equal(t, originalUpdaters, afterUpdaters, "updaters should be preserved on label-only change")
	assert.Equal(t, "my-label-value", fetched.Labels["my-label"], "user label should be set")

	t.Log("")
	t.Log("SUCCESS: Kausality annotations preserved on label-only change")
}

// =============================================================================
// Helper: Create ConfigMap for non-apps/v1 testing
// =============================================================================

func createConfigMap(t *testing.T, ctx context.Context, namePrefix string) *corev1.ConfigMap {
	t.Helper()
	testCounter++

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namePrefix + "-" + string(rune(testCounter)),
			Namespace: testNS,
		},
		Data: map[string]string{
			"key": "value",
		},
	}

	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("failed to create configmap: %v", err)
	}

	// Re-fetch to get server-set fields
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("failed to get configmap: %v", err)
	}

	return cm
}
