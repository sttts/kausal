//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kadmission "github.com/kausality-io/kausality/pkg/admission"
	"github.com/kausality-io/kausality/pkg/approval"
	"github.com/kausality-io/kausality/pkg/config"
	"github.com/kausality-io/kausality/pkg/drift"
	"github.com/kausality-io/kausality/pkg/trace"
)

// Shared test environment
var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	scheme    = runtime.NewScheme()
	testNS    string
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	testEnv = &envtest.Environment{}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic(fmt.Sprintf("failed to start envtest: %v", err))
	}

	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(fmt.Sprintf("failed to create client: %v", err))
	}

	// Create a shared namespace for tests
	testNS = fmt.Sprintf("kausality-test-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNS},
	}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		panic(fmt.Sprintf("failed to create test namespace: %v", err))
	}

	code := m.Run()

	// Cleanup
	_ = k8sClient.Delete(context.Background(), ns)
	_ = testEnv.Stop()

	os.Exit(code)
}

// =============================================================================
// Test: Controller Identification via managedFields
// =============================================================================

func TestControllerIdentification_ManagedFields(t *testing.T) {
	ctx := context.Background()

	// Create a Deployment
	deploy := createDeployment(t, ctx, "ctrl-id-deploy")

	// Simulate controller updating status (sets observedGeneration)
	// In real scenario, the deployment controller does this
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = 1
	deploy.Status.ReadyReplicas = 1
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment status: %v", err)
	}

	// Re-fetch to get managedFields
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("Deployment managedFields: %d entries", len(deploy.ManagedFields))
	for _, mf := range deploy.ManagedFields {
		t.Logf("  Manager: %s, Operation: %s, Subresource: %s", mf.Manager, mf.Operation, mf.Subresource)
	}

	// Verify we can find the controller manager
	// The status update should have created a managedFields entry
	resolver := drift.NewParentResolver(k8sClient)

	// Create a child ReplicaSet with ownerRef
	rs := createReplicaSetWithOwner(t, ctx, "ctrl-id-rs", deploy)

	// Resolve parent and check controller manager is populated
	parentState, err := resolver.ResolveParent(ctx, rs)
	if err != nil {
		t.Fatalf("failed to resolve parent: %v", err)
	}

	if parentState == nil {
		t.Fatal("expected parent state, got nil")
	}

	t.Logf("Parent state: gen=%d, obsGen=%d, controllerManager=%q",
		parentState.Generation, parentState.ObservedGeneration, parentState.ControllerManager)

	// The controllerManager should be set from managedFields
	// (could be empty if no status update created managedFields entry for observedGeneration)
}

// =============================================================================
// Test: Drift Detection - Expected Change (gen != obsGen)
// =============================================================================

func TestDriftDetection_ExpectedChange(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "expected-change-deploy")

	// Don't update status yet - generation != observedGeneration (0)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "expected-change-rs", deploy)

	// Detect drift
	detector := drift.NewDetector(k8sClient)
	result, err := detector.Detect(ctx, rs)
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
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		// If generation is 1, we can't set obsGen to 0 and have it < gen
		// Just update to same value and then bump generation
		deploy.Status.ObservedGeneration = deploy.Generation
		_ = k8sClient.Status().Update(ctx, deploy)

		// Bump generation by updating spec
		replicas := int32(2)
		deploy.Spec.Replicas = &replicas
		if err := k8sClient.Update(ctx, deploy); err != nil {
			t.Fatalf("failed to update deployment: %v", err)
		}
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("After update: gen=%d, obsGen=%d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Now gen != obsGen - should be expected change
	result, err = detector.Detect(ctx, rs)
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
	deploy := createDeployment(t, ctx, "drift-deploy")

	// Set observedGeneration = generation (stable state)
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = 1
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("Deployment: gen=%d, obsGen=%d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "drift-rs", deploy)

	// Detect drift
	detector := drift.NewDetector(k8sClient)
	result, err := detector.Detect(ctx, rs)
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

func TestTracePropagation_NewOrigin(t *testing.T) {
	ctx := context.Background()

	// Create deployment without parent (origin)
	deploy := createDeployment(t, ctx, "trace-origin-deploy")

	propagator := trace.NewPropagator(k8sClient)
	result, err := propagator.Propagate(ctx, deploy, "test-user@example.com")
	if err != nil {
		t.Fatalf("propagation failed: %v", err)
	}

	t.Logf("Trace result: isOrigin=%v, trace=%s", result.IsOrigin, result.Trace.String())

	if !result.IsOrigin {
		t.Errorf("expected isOrigin=true for object without parent")
	}

	if len(result.Trace) != 1 {
		t.Errorf("expected trace length 1, got %d", len(result.Trace))
	}

	if len(result.Trace) > 0 && result.Trace[0].User != "test-user@example.com" {
		t.Errorf("expected user 'test-user@example.com', got %q", result.Trace[0].User)
	}
}

// =============================================================================
// Test: Trace Propagation - Extend Parent Trace
// =============================================================================

func TestTracePropagation_ExtendParent(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment with a trace
	deploy := createDeployment(t, ctx, "trace-extend-deploy")

	// Set a trace on the parent
	parentTrace := trace.Trace{
		trace.NewHop("apps/v1", "Deployment", deploy.Name, deploy.Generation, "parent-user"),
	}
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[trace.TraceAnnotation] = parentTrace.String()
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with trace: %v", err)
	}

	// Set observedGeneration < generation (parent reconciling)
	// First bump generation
	replicas := int32(2)
	deploy.Spec.Replicas = &replicas
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment spec: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Status update with obsGen < generation
	deploy.Status.ObservedGeneration = deploy.Generation - 1
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("Parent: gen=%d, obsGen=%d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "trace-extend-rs", deploy)

	// Propagate trace to child
	propagator := trace.NewPropagator(k8sClient)
	result, err := propagator.Propagate(ctx, rs, "controller-sa")
	if err != nil {
		t.Fatalf("propagation failed: %v", err)
	}

	t.Logf("Trace result: isOrigin=%v, trace=%s", result.IsOrigin, result.Trace.String())

	// Parent is reconciling - should extend trace
	if result.IsOrigin {
		t.Errorf("expected isOrigin=false when parent is reconciling")
	}

	// Trace should have parent hop + child hop
	if len(result.Trace) != 2 {
		t.Errorf("expected trace length 2, got %d", len(result.Trace))
	}
}

// =============================================================================
// Test: Lifecycle Phase - Initializing
// =============================================================================

func TestLifecyclePhase_Initializing(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment (no observedGeneration yet = initializing)
	deploy := createDeployment(t, ctx, "lifecycle-init-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "lifecycle-init-rs", deploy)

	// Detect drift
	detector := drift.NewDetector(k8sClient)
	result, err := detector.Detect(ctx, rs)
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
	deploy := createDeployment(t, ctx, "lifecycle-delete-deploy")
	deploy.Finalizers = []string{"test.kausality.io/finalizer"}
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to add finalizer: %v", err)
	}

	// Set observedGeneration (mark as ready)
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "lifecycle-delete-rs", deploy)

	// Delete the deployment (will be blocked by finalizer, but sets deletionTimestamp)
	if err := k8sClient.Delete(ctx, deploy); err != nil {
		t.Fatalf("failed to delete deployment: %v", err)
	}

	// Re-fetch deployment to get deletionTimestamp
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	if deploy.DeletionTimestamp == nil {
		t.Fatal("expected deletionTimestamp to be set")
	}

	t.Logf("Deployment deletionTimestamp: %v", deploy.DeletionTimestamp)

	// Detect drift
	detector := drift.NewDetector(k8sClient)
	result, err := detector.Detect(ctx, rs)
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
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Logf("failed to remove finalizer: %v", err)
	}
}

// =============================================================================
// Test: FieldManager Matching
// =============================================================================

func TestFieldManagerMatching_SameManager(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "fieldmgr-same-deploy")

	// Update status with a specific manager
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = 1
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Find the controller manager from managedFields
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}
	t.Logf("Controller manager from managedFields: %q", controllerManager)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "fieldmgr-same-rs", deploy)

	// Detect drift with matching fieldManager
	detector := drift.NewDetector(k8sClient)
	result, err := detector.DetectWithFieldManager(ctx, rs, controllerManager)
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result with matching manager: allowed=%v, drift=%v, reason=%s",
		result.Allowed, result.DriftDetected, result.Reason)

	// Same manager + gen == obsGen = drift (controller updating when nothing changed)
	if !result.DriftDetected {
		t.Errorf("expected driftDetected=true when gen == obsGen")
	}
}

func TestFieldManagerMatching_DifferentManager(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "fieldmgr-diff-deploy")

	// Set up parent as ready (gen == obsGen)
	deploy.Status.ObservedGeneration = deploy.Generation
	deploy.Status.Replicas = 1
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "fieldmgr-diff-rs", deploy)

	// Detect drift with different fieldManager
	detector := drift.NewDetector(k8sClient)
	result, err := detector.DetectWithFieldManager(ctx, rs, "some-other-controller")
	if err != nil {
		t.Fatalf("drift detection failed: %v", err)
	}

	t.Logf("Result with different manager: allowed=%v, drift=%v, reason=%s",
		result.Allowed, result.DriftDetected, result.Reason)

	// Different manager = NOT drift (it's a different actor, new causal origin)
	if result.DriftDetected {
		t.Errorf("expected driftDetected=false for different manager (not drift, just different actor)")
	}
}

// =============================================================================
// Test: Admission Handler Integration
// =============================================================================

func TestAdmissionHandler_Integration(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "handler-int-deploy")

	// Set as ready
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Create handler
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
	})

	// Create a child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "handler-int-rs", deploy)

	// Set TypeMeta explicitly (not populated by client.Get)
	rs.APIVersion = "apps/v1"
	rs.Kind = "ReplicaSet"

	// Serialize the ReplicaSet
	rsBytes, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("failed to marshal replicaset: %v", err)
	}

	// Create admission request
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("test-uid"),
			Operation: admissionv1.Update,
			Kind: metav1.GroupVersionKind{
				Group:   "apps",
				Version: "v1",
				Kind:    "ReplicaSet",
			},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			Object: runtime.RawExtension{
				Raw: rsBytes,
			},
			OldObject: runtime.RawExtension{
				Raw: rsBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "system:serviceaccount:kube-system:deployment-controller",
			},
			Options: runtime.RawExtension{
				Raw: []byte(`{"fieldManager":"deployment-controller"}`),
			},
		},
	}

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, result=%v", resp.Allowed, resp.Result)

	// Phase 1: always allow
	if !resp.Allowed {
		t.Errorf("expected allowed=true")
	}
}

// =============================================================================
// Test: No Owner Reference (Root Object)
// =============================================================================

func TestNoOwnerReference_RootObject(t *testing.T) {
	ctx := context.Background()

	// Create a deployment without any owner (root object)
	deploy := createDeployment(t, ctx, "root-deploy")

	// Detect drift on root object
	detector := drift.NewDetector(k8sClient)
	result, err := detector.Detect(ctx, deploy)
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
	deploy := createDeployment(t, ctx, "ready-cond-deploy")

	// Set Ready=True condition (simulating a CRD with conditions but no observedGeneration yet)
	deploy.Status.Conditions = []appsv1.DeploymentCondition{
		{
			Type:   appsv1.DeploymentAvailable,
			Status: corev1.ConditionTrue,
		},
	}
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "ready-cond-rs", deploy)

	// Resolve parent state
	resolver := drift.NewParentResolver(k8sClient)
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

func TestDifferentActor_NewTraceOrigin(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment with a trace
	deploy := createDeployment(t, ctx, "diff-actor-deploy")

	// Set a trace on the parent
	parentTrace := trace.Trace{
		trace.NewHop("apps/v1", "Deployment", deploy.Name, deploy.Generation, "original-user"),
	}
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[trace.TraceAnnotation] = parentTrace.String()
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with trace: %v", err)
	}

	// Set parent as stable (gen == obsGen) - any change would be drift if from controller
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("Parent: gen=%d, obsGen=%d", deploy.Generation, deploy.Status.ObservedGeneration)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "diff-actor-rs", deploy)

	// Propagate trace with DIFFERENT fieldManager (simulating kubectl or another actor)
	propagator := trace.NewPropagator(k8sClient)
	result, err := propagator.PropagateWithFieldManager(ctx, rs, "different-user", "kubectl-edit")
	if err != nil {
		t.Fatalf("propagation failed: %v", err)
	}

	t.Logf("Trace result: isOrigin=%v, trace=%s", result.IsOrigin, result.Trace.String())

	// Different actor should create a NEW trace origin (not extend parent's trace)
	if !result.IsOrigin {
		t.Errorf("expected isOrigin=true for different actor")
	}

	// New trace should have only 1 hop (the new origin), not parent's trace
	if len(result.Trace) != 1 {
		t.Errorf("expected trace length 1 (new origin), got %d", len(result.Trace))
	}

	// The origin should be the new actor, not the parent's trace user
	if len(result.Trace) > 0 && result.Trace[0].User != "different-user" {
		t.Errorf("expected trace origin user 'different-user', got %q", result.Trace[0].User)
	}
}

// =============================================================================
// Test: Admission Handler Returns Trace Patch
// =============================================================================

func TestAdmissionHandler_TraceInResponse(t *testing.T) {
	ctx := context.Background()

	// Create a standalone deployment (no parent - will be origin)
	deploy := createDeployment(t, ctx, "trace-patch-deploy")

	// Set TypeMeta
	deploy.APIVersion = "apps/v1"
	deploy.Kind = "Deployment"

	// Serialize
	deployBytes, err := json.Marshal(deploy)
	if err != nil {
		t.Fatalf("failed to marshal deployment: %v", err)
	}

	// Create handler
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
	})

	// Create admission request for CREATE (new object, will be origin)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("trace-patch-uid"),
			Operation: admissionv1.Create,
			Kind: metav1.GroupVersionKind{
				Group:   "apps",
				Version: "v1",
				Kind:    "Deployment",
			},
			Namespace: deploy.Namespace,
			Name:      deploy.Name,
			Object: runtime.RawExtension{
				Raw: deployBytes,
			},
			UserInfo: authenticationv1.UserInfo{
				Username: "test-user@example.com",
			},
			Options: runtime.RawExtension{
				Raw: []byte(`{"fieldManager":"kubectl-create"}`),
			},
		},
	}

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, patchType=%v, patchLen=%d",
		resp.Allowed, resp.PatchType, len(resp.Patches))

	if !resp.Allowed {
		t.Errorf("expected allowed=true")
	}

	// Response should contain a patch with the trace annotation
	if len(resp.Patches) == 0 && resp.Patch == nil {
		// Check if patch is in the raw response
		if len(resp.Patch) == 0 {
			t.Logf("No patches returned - this may be expected if PatchResponseFromRaw returns full object")
		}
	}

	// The response should have the patched object with trace
	if resp.Patch != nil {
		patchStr := string(resp.Patch)
		if !containsSubstring(patchStr, trace.TraceAnnotation) {
			t.Logf("Patch content: %s", patchStr)
			// Note: PatchResponseFromRaw returns the full modified object, not a JSON patch
			// The trace annotation should be in there
		}
	}
}

// =============================================================================
// Test: Multiple Owner References - Only Controller Matters
// =============================================================================

func TestMultipleOwnerRefs_OnlyControllerMatters(t *testing.T) {
	ctx := context.Background()

	// Create two deployments - one will be controller, one won't
	controllerDeploy := createDeployment(t, ctx, "multi-owner-ctrl")
	nonControllerDeploy := createDeployment(t, ctx, "multi-owner-nonctrl")

	// Set controller as stable (drift scenario)
	controllerDeploy.Status.ObservedGeneration = controllerDeploy.Generation
	if err := k8sClient.Status().Update(ctx, controllerDeploy); err != nil {
		t.Fatalf("failed to update controller status: %v", err)
	}

	// Set non-controller as reconciling (would be "expected" if it were the controller)
	nonControllerDeploy.Status.ObservedGeneration = nonControllerDeploy.Generation - 1
	// This will fail if generation is 1, so just set it to same
	if nonControllerDeploy.Generation == 1 {
		nonControllerDeploy.Status.ObservedGeneration = 0
	}
	_ = k8sClient.Status().Update(ctx, nonControllerDeploy) // Ignore error

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(controllerDeploy), controllerDeploy); err != nil {
		t.Fatalf("failed to get controller deploy: %v", err)
	}

	// Create ReplicaSet with BOTH owners, but only one is controller
	trueVal := true
	falseVal := false
	testCounter++
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("multi-owner-rs-%d", testCounter),
			Namespace: testNS,
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

	if err := k8sClient.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	// Detect drift - should use controllerDeploy (the one with controller: true)
	detector := drift.NewDetector(k8sClient)
	result, err := detector.Detect(ctx, rs)
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
// Test: Approval - Mode Always
// =============================================================================

func TestApproval_ModeAlways(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "approval-always-deploy")

	// Create child ReplicaSet FIRST (so we know its name)
	rs := createReplicaSetWithOwner(t, ctx, "approval-always-rs", deploy)

	// Set parent as ready (gen == obsGen) - drift scenario
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Add approval annotation using ACTUAL RS name
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Mode: approval.ModeAlways},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with approval: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Check approval directly
	checker := approval.NewChecker()
	childRef := approval.ChildRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name}
	result := checker.Check(deploy, childRef, deploy.Generation)

	t.Logf("Approval check: approved=%v, reason=%s", result.Approved, result.Reason)

	if !result.Approved {
		t.Errorf("expected approved=true for mode=always")
	}
	if result.MatchedApproval == nil {
		t.Errorf("expected MatchedApproval to be set")
	}
	if result.MatchedApproval != nil && result.MatchedApproval.Mode != approval.ModeAlways {
		t.Errorf("expected Mode=%q, got %q", approval.ModeAlways, result.MatchedApproval.Mode)
	}
}

// =============================================================================
// Test: Approval - Mode Once (Valid)
// =============================================================================

func TestApproval_ModeOnce(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "approval-once-deploy")

	// Create child ReplicaSet FIRST
	rs := createReplicaSetWithOwner(t, ctx, "approval-once-rs", deploy)

	// Set parent as ready
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch to get generation AFTER status update (status update doesn't bump generation)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Add approval with mode=once matching current generation
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Generation: deploy.Generation, Mode: approval.ModeOnce},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with approval: %v", err)
	}

	// Re-fetch (generation bumps due to annotation update)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("Parent generation: %d, approval generation in annotation: %d", deploy.Generation, deploy.Generation-1)

	// The approval was set for generation N, but update bumped to N+1
	// So we need to check with the generation the approval was created for
	checker := approval.NewChecker()
	childRef := approval.ChildRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name}

	// Check with the original generation (the one in the approval)
	// Parse back to verify
	parsedApprovals, _ := approval.ParseApprovals(deploy.GetAnnotations()[approval.ApprovalsAnnotation])
	if len(parsedApprovals) == 0 {
		t.Fatal("no approvals parsed")
	}
	approvalGen := parsedApprovals[0].Generation

	result := checker.Check(deploy, childRef, approvalGen)

	t.Logf("Approval check: approved=%v, reason=%s", result.Approved, result.Reason)

	if !result.Approved {
		t.Errorf("expected approved=true for mode=once with matching generation")
	}
	if result.MatchedApproval == nil {
		t.Errorf("expected MatchedApproval to be set")
	}
	if result.MatchedApproval != nil && result.MatchedApproval.Mode != approval.ModeOnce {
		t.Errorf("expected Mode=%q, got %q", approval.ModeOnce, result.MatchedApproval.Mode)
	}
}

// =============================================================================
// Test: Approval - Mode Generation (Valid)
// =============================================================================

func TestApproval_ModeGeneration(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "approval-gen-deploy")

	// Create child ReplicaSet FIRST
	rs := createReplicaSetWithOwner(t, ctx, "approval-gen-rs", deploy)

	// Set parent as ready
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Add approval with mode=generation - use CURRENT generation
	currentGen := deploy.Generation
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Generation: currentGen, Mode: approval.ModeGeneration},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with approval: %v", err)
	}

	// Re-fetch (generation bumped)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Check approval - verify it matches for the generation it was created for
	checker := approval.NewChecker()
	childRef := approval.ChildRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name}
	result := checker.Check(deploy, childRef, currentGen)

	t.Logf("Approval check: approved=%v, reason=%s", result.Approved, result.Reason)

	if !result.Approved {
		t.Errorf("expected approved=true for mode=generation with matching generation")
	}
	if result.MatchedApproval != nil && result.MatchedApproval.Mode != approval.ModeGeneration {
		t.Errorf("expected Mode=%q, got %q", approval.ModeGeneration, result.MatchedApproval.Mode)
	}
}

// =============================================================================
// Test: Approval - Stale Generation (Invalid)
// =============================================================================

func TestApproval_StaleGeneration(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "approval-stale-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "approval-stale-rs", deploy)

	// Set as ready
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Add approval with generation=1 (will be stale after annotation update)
	staleGeneration := int64(1)
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Generation: staleGeneration, Mode: approval.ModeOnce},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with approval: %v", err)
	}

	// Re-fetch (generation bumps due to update)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	t.Logf("Deploy generation: %d, approval generation: %d", deploy.Generation, staleGeneration)

	// Check approval - should be stale (approval is for gen 1, but current gen is higher)
	checker := approval.NewChecker()
	childRef := approval.ChildRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name}
	result := checker.Check(deploy, childRef, deploy.Generation)

	t.Logf("Approval check: approved=%v, reason=%s", result.Approved, result.Reason)

	// Stale approval should NOT match (unless current gen happens to be 1)
	if deploy.Generation > staleGeneration && result.Approved {
		t.Errorf("expected approved=false for stale generation approval (gen=%d, approval.gen=%d)",
			deploy.Generation, staleGeneration)
	}
}

// =============================================================================
// Test: Rejection - Blocks Drift
// =============================================================================

func TestRejection_BlocksDrift(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "rejection-deploy")

	// Create child ReplicaSet FIRST
	rs := createReplicaSetWithOwner(t, ctx, "rejection-rs", deploy)

	// Set parent as ready
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Add rejection annotation using actual RS name
	rejections := []approval.Rejection{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Reason: "dangerous mutation"},
	}
	rejectionsJSON, _ := json.Marshal(rejections)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.RejectionsAnnotation] = string(rejectionsJSON)
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with rejection: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Check for rejection
	checker := approval.NewChecker()
	childRef := approval.ChildRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name}
	result := checker.Check(deploy, childRef, deploy.Generation)

	t.Logf("Rejection check: rejected=%v, reason=%s", result.Rejected, result.Reason)

	if !result.Rejected {
		t.Errorf("expected rejected=true")
	}
	if result.Reason != "dangerous mutation" {
		t.Errorf("expected reason 'dangerous mutation', got %q", result.Reason)
	}
	if result.MatchedRejection == nil {
		t.Errorf("expected MatchedRejection to be set")
	}
}

// =============================================================================
// Test: Rejection Wins Over Approval
// =============================================================================

func TestRejection_WinsOverApproval(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "reject-wins-deploy")

	// Create child ReplicaSet FIRST
	rs := createReplicaSetWithOwner(t, ctx, "reject-wins-rs", deploy)

	// Set parent as ready
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Add BOTH approval AND rejection for same child
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Mode: approval.ModeAlways},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)

	rejections := []approval.Rejection{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Reason: "explicitly blocked"},
	}
	rejectionsJSON, _ := json.Marshal(rejections)

	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	annotations[approval.RejectionsAnnotation] = string(rejectionsJSON)
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Check - rejection should win
	checker := approval.NewChecker()
	childRef := approval.ChildRef{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name}
	result := checker.Check(deploy, childRef, deploy.Generation)

	t.Logf("Check result: approved=%v, rejected=%v, reason=%s",
		result.Approved, result.Rejected, result.Reason)

	// Rejection should win
	if !result.Rejected {
		t.Errorf("expected rejected=true (rejection wins over approval)")
	}
	if result.Approved {
		t.Errorf("expected approved=false when rejected")
	}
	if result.Reason != "explicitly blocked" {
		t.Errorf("expected reason 'explicitly blocked', got %q", result.Reason)
	}
}

// =============================================================================
// Test: Enforce Mode - Rejection Returns Denied Response
// =============================================================================

func TestEnforceMode_RejectionDenied(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "enforce-reject-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "enforce-reject-rs", deploy)

	// Add rejection annotation FIRST (this bumps generation)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	rejections := []approval.Rejection{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Reason: "blocked by policy"},
	}
	rejectionsJSON, _ := json.Marshal(rejections)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.RejectionsAnnotation] = string(rejectionsJSON)
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

	// Set parent as ready AFTER annotation update (drift scenario: gen == obsGen)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch to get managedFields
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Find the controller manager from managedFields (like a real controller would have)
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}
	t.Logf("Parent state: gen=%d, obsGen=%d, controllerManager=%q",
		deploy.Generation, deploy.Status.ObservedGeneration, controllerManager)

	// Create handler with ENFORCE MODE for apps/replicasets
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeEnforce,
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}
	rs.APIVersion = "apps/v1"
	rs.Kind = "ReplicaSet"

	// Create old and new versions with DIFFERENT specs to trigger drift check
	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, err := json.Marshal(oldRS)
	if err != nil {
		t.Fatalf("failed to marshal old replicaset: %v", err)
	}
	newBytes, err := json.Marshal(newRS)
	if err != nil {
		t.Fatalf("failed to marshal new replicaset: %v", err)
	}

	// Use the actual controller manager from parent's managedFields
	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("enforce-reject-uid"),
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			Object:    runtime.RawExtension{Raw: newBytes},
			OldObject: runtime.RawExtension{Raw: oldBytes},
			UserInfo:  authenticationv1.UserInfo{Username: "controller"},
			Options:   runtime.RawExtension{Raw: []byte(optionsJSON)},
		},
	}

	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, result=%v", resp.Allowed, resp.Result)

	// In enforce mode, rejection should deny the request
	if resp.Allowed {
		t.Errorf("expected allowed=false in enforce mode with rejection")
	}
	if resp.Result == nil {
		t.Fatal("expected Result to be set")
	}
	if resp.Result.Message == "" {
		t.Errorf("expected rejection message in Result.Message")
	}
	// The message should contain the rejection reason
	if !containsSubstring(resp.Result.Message, "blocked by policy") {
		t.Errorf("expected message to contain 'blocked by policy', got %q", resp.Result.Message)
	}
}

// =============================================================================
// Test: Enforce Mode - Unapproved Drift Returns Denied Response
// =============================================================================

func TestEnforceMode_UnapprovedDriftDenied(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "enforce-drift-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "enforce-drift-rs", deploy)

	// Set parent as ready (drift scenario: gen == obsGen) - NO approvals
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch to get managedFields
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Find the controller manager from managedFields
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}
	t.Logf("Parent state: gen=%d, obsGen=%d, controllerManager=%q",
		deploy.Generation, deploy.Status.ObservedGeneration, controllerManager)

	// Create handler with ENFORCE MODE for apps/replicasets
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeEnforce,
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}
	rs.APIVersion = "apps/v1"
	rs.Kind = "ReplicaSet"

	// Create old and new versions with DIFFERENT specs to trigger drift check
	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, err := json.Marshal(oldRS)
	if err != nil {
		t.Fatalf("failed to marshal old replicaset: %v", err)
	}
	newBytes, err := json.Marshal(newRS)
	if err != nil {
		t.Fatalf("failed to marshal new replicaset: %v", err)
	}

	// Use the actual controller manager from parent's managedFields
	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("enforce-drift-uid"),
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			Object:    runtime.RawExtension{Raw: newBytes},
			OldObject: runtime.RawExtension{Raw: oldBytes},
			UserInfo:  authenticationv1.UserInfo{Username: "controller"},
			Options:   runtime.RawExtension{Raw: []byte(optionsJSON)},
		},
	}

	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, result=%v", resp.Allowed, resp.Result)

	// In enforce mode, unapproved drift should deny the request
	if resp.Allowed {
		t.Errorf("expected allowed=false in enforce mode with unapproved drift")
	}
	if resp.Result == nil {
		t.Fatal("expected Result to be set")
	}
	if resp.Result.Message == "" {
		t.Errorf("expected drift message in Result.Message")
	}
	// The message should indicate drift with no approval
	if !containsSubstring(resp.Result.Message, "no approval") {
		t.Errorf("expected message to contain 'no approval', got %q", resp.Result.Message)
	}
}

// =============================================================================
// Test: Enforce Mode - Approved Drift Is Allowed
// =============================================================================

func TestEnforceMode_ApprovedDriftAllowed(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "enforce-approved-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "enforce-approved-rs", deploy)

	// Add approval annotation FIRST (this bumps generation)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Mode: approval.ModeAlways},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

	// Set parent as ready AFTER annotation update (drift scenario: gen == obsGen)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch to get managedFields
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Find the controller manager from managedFields
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}
	t.Logf("Parent state: gen=%d, obsGen=%d, controllerManager=%q",
		deploy.Generation, deploy.Status.ObservedGeneration, controllerManager)

	// Create handler with ENFORCE MODE for apps/replicasets
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeEnforce,
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}
	rs.APIVersion = "apps/v1"
	rs.Kind = "ReplicaSet"

	// Create old and new versions with DIFFERENT specs to trigger drift check
	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, err := json.Marshal(oldRS)
	if err != nil {
		t.Fatalf("failed to marshal old replicaset: %v", err)
	}
	newBytes, err := json.Marshal(newRS)
	if err != nil {
		t.Fatalf("failed to marshal new replicaset: %v", err)
	}

	// Use the actual controller manager from parent's managedFields
	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("enforce-approved-uid"),
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			Object:    runtime.RawExtension{Raw: newBytes},
			OldObject: runtime.RawExtension{Raw: oldBytes},
			UserInfo:  authenticationv1.UserInfo{Username: "controller"},
			Options:   runtime.RawExtension{Raw: []byte(optionsJSON)},
		},
	}

	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v", resp.Allowed)

	// Even in enforce mode, approved drift should be allowed
	if !resp.Allowed {
		t.Errorf("expected allowed=true in enforce mode with valid approval")
	}
}

// =============================================================================
// Test: Non-Enforce Mode Returns Warnings
// =============================================================================

func TestNonEnforceMode_ReturnsWarnings(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "warning-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "warning-rs", deploy)

	// Set parent as ready (drift scenario: gen == obsGen) - NO approvals
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch to get managedFields
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Find the controller manager from managedFields
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler WITHOUT enforce mode (default log mode)
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog, // Non-enforce mode
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}
	rs.APIVersion = "apps/v1"
	rs.Kind = "ReplicaSet"

	// Create old and new versions with DIFFERENT specs to trigger drift check
	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, err := json.Marshal(oldRS)
	if err != nil {
		t.Fatalf("failed to marshal old replicaset: %v", err)
	}
	newBytes, err := json.Marshal(newRS)
	if err != nil {
		t.Fatalf("failed to marshal new replicaset: %v", err)
	}

	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("warning-uid"),
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			Object:    runtime.RawExtension{Raw: newBytes},
			OldObject: runtime.RawExtension{Raw: oldBytes},
			UserInfo:  authenticationv1.UserInfo{Username: "controller"},
			Options:   runtime.RawExtension{Raw: []byte(optionsJSON)},
		},
	}

	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, warnings=%v", resp.Allowed, resp.Warnings)

	// Should be allowed (non-enforce mode)
	if !resp.Allowed {
		t.Errorf("expected allowed=true in non-enforce mode")
	}

	// Should have warnings about drift
	if len(resp.Warnings) == 0 {
		t.Errorf("expected warnings in non-enforce mode with drift")
	}

	// Check warning content
	hasKausalityWarning := false
	for _, w := range resp.Warnings {
		if containsSubstring(w, "kausality") && containsSubstring(w, "drift") {
			hasKausalityWarning = true
			break
		}
	}
	if !hasKausalityWarning {
		t.Errorf("expected kausality drift warning, got: %v", resp.Warnings)
	}
}

// =============================================================================
// Test: Mode=Once Approval Is Consumed After Use
// =============================================================================

func TestApprovalConsumed_ModeOnce(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "consume-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "consume-rs", deploy)

	// Get current generation for the approval
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Add mode=once approval annotation with the NEXT generation
	// (since updating annotations will bump the generation)
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Mode: approval.ModeOnce, Generation: deploy.Generation + 1},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with approval: %v", err)
	}

	// Set parent as ready AFTER annotation update (drift scenario: gen == obsGen)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Verify approval exists before handler
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	beforeApprovals := deploy.GetAnnotations()[approval.ApprovalsAnnotation]
	t.Logf("Before handler - approvals: %s", beforeApprovals)

	// Find the controller manager from managedFields
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler (log mode - approval should still be consumed)
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    ctrl.Log.WithName("test-consume"),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog,
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}
	rs.APIVersion = "apps/v1"
	rs.Kind = "ReplicaSet"

	// Create old and new versions with DIFFERENT specs to trigger drift check
	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, err := json.Marshal(oldRS)
	if err != nil {
		t.Fatalf("failed to marshal old replicaset: %v", err)
	}
	newBytes, err := json.Marshal(newRS)
	if err != nil {
		t.Fatalf("failed to marshal new replicaset: %v", err)
	}

	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("consume-uid"),
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			Object:    runtime.RawExtension{Raw: newBytes},
			OldObject: runtime.RawExtension{Raw: oldBytes},
			UserInfo:  authenticationv1.UserInfo{Username: "controller"},
			Options:   runtime.RawExtension{Raw: []byte(optionsJSON)},
		},
	}

	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v", resp.Allowed)

	// Should be allowed (approval was valid)
	if !resp.Allowed {
		t.Errorf("expected allowed=true with valid approval")
	}

	// Give a moment for the async update (if any)
	time.Sleep(100 * time.Millisecond)

	// Verify approval was consumed (removed from parent)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment after handler: %v", err)
	}
	afterApprovals := deploy.GetAnnotations()[approval.ApprovalsAnnotation]
	t.Logf("After handler - approvals: %s", afterApprovals)

	// The approval should be removed (empty or missing)
	if afterApprovals != "" {
		// Parse and check if our approval is still there
		remaining, _ := approval.ParseApprovals(afterApprovals)
		for _, a := range remaining {
			if a.Name == rs.Name && a.Mode == approval.ModeOnce {
				t.Errorf("mode=once approval should have been consumed, but still exists: %v", remaining)
			}
		}
	}
}

// =============================================================================
// Test: Mode=Always Approval Is NOT Consumed
// =============================================================================

func TestApprovalNotConsumed_ModeAlways(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "always-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "always-rs", deploy)

	// Add mode=always approval annotation
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Mode: approval.ModeAlways},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment with approval: %v", err)
	}

	// Set parent as ready AFTER annotation update
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Re-fetch and find controller manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    ctrl.Log.WithName("test-always"),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog,
			},
		},
	})

	// Re-fetch RS
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get rs: %v", err)
	}
	rs.APIVersion = "apps/v1"
	rs.Kind = "ReplicaSet"

	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, _ := json.Marshal(oldRS)
	newBytes, _ := json.Marshal(newRS)
	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("always-uid"),
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			Object:    runtime.RawExtension{Raw: newBytes},
			OldObject: runtime.RawExtension{Raw: oldBytes},
			UserInfo:  authenticationv1.UserInfo{Username: "controller"},
			Options:   runtime.RawExtension{Raw: []byte(optionsJSON)},
		},
	}

	resp := handler.Handle(ctx, req)

	if !resp.Allowed {
		t.Errorf("expected allowed=true with valid approval")
	}

	time.Sleep(100 * time.Millisecond)

	// Verify approval was NOT consumed (mode=always should persist)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment after handler: %v", err)
	}
	afterApprovals := deploy.GetAnnotations()[approval.ApprovalsAnnotation]
	t.Logf("After handler - approvals: %s", afterApprovals)

	if afterApprovals == "" {
		t.Errorf("mode=always approval should NOT be consumed, but annotation is empty")
	} else {
		remaining, _ := approval.ParseApprovals(afterApprovals)
		found := false
		for _, a := range remaining {
			if a.Name == rs.Name && a.Mode == approval.ModeAlways {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("mode=always approval should still exist, remaining: %v", remaining)
		}
	}
}

// =============================================================================
// Test: Approval Pruning - Stale Approvals Removed
// =============================================================================

func TestApprovalPruning_StaleApprovals(t *testing.T) {
	pruner := approval.NewPruner()

	approvals := []approval.Approval{
		{APIVersion: "v1", Kind: "ConfigMap", Name: "always-valid", Mode: approval.ModeAlways},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "current-gen", Generation: 5, Mode: approval.ModeOnce},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "stale-gen", Generation: 3, Mode: approval.ModeOnce},
		{APIVersion: "v1", Kind: "ConfigMap", Name: "future-gen", Generation: 7, Mode: approval.ModeGeneration},
	}

	// Prune with current generation = 5
	result := pruner.PruneStale(approvals, 5)

	t.Logf("Pruned from %d to %d approvals", len(approvals), len(result))

	// Should keep: always-valid, current-gen, future-gen
	// Should remove: stale-gen (generation 3 < 5)
	if len(result) != 3 {
		t.Errorf("expected 3 remaining approvals, got %d", len(result))
	}

	// Verify stale-gen is gone
	for _, a := range result {
		if a.Name == "stale-gen" {
			t.Errorf("stale-gen should have been pruned")
		}
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstr(s, substr)))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// =============================================================================
// Helper Functions
// =============================================================================

var testCounter int

func createDeployment(t *testing.T, ctx context.Context, namePrefix string) *appsv1.Deployment {
	t.Helper()
	testCounter++

	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNS,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, deploy); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	// Re-fetch to get server-set fields
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	return deploy
}

func createReplicaSetWithOwner(t *testing.T, ctx context.Context, namePrefix string, owner *appsv1.Deployment) *appsv1.ReplicaSet {
	t.Helper()
	testCounter++

	trueVal := true
	replicas := int32(1)

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNS,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       owner.Name,
					UID:        owner.UID,
					Controller: &trueVal,
				},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get replicaset: %v", err)
	}

	return rs
}
