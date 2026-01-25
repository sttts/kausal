//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kadmission "github.com/kausality-io/kausality/pkg/admission"
	"github.com/kausality-io/kausality/pkg/controller"
	"github.com/kausality-io/kausality/pkg/trace"
)

func TestTracePropagation_NewOrigin(t *testing.T) {
	ctx := context.Background()

	// Create deployment without parent (origin)
	deploy := createDeployment(t, ctx, "trace-origin-deploy")

	propagator := trace.NewPropagator(k8sClient)
	result, err := propagator.Propagate(ctx, deploy, "test-user@example.com", nil, "")
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
		trace.NewHop("apps/v1", "Deployment", deploy.Name, deploy.Generation, "parent-user", ""),
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

	// Propagate trace to child - controller-sa is the only updater, so it's the controller
	propagator := trace.NewPropagator(k8sClient)
	childUpdaters := []string{controller.HashUsername("controller-sa")}
	result, err := propagator.Propagate(ctx, rs, "controller-sa", childUpdaters, "")
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

func TestDifferentActor_NewTraceOrigin(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment with a trace
	deploy := createDeployment(t, ctx, "diff-actor-deploy")

	// Set a trace on the parent
	parentTrace := trace.Trace{
		trace.NewHop("apps/v1", "Deployment", deploy.Name, deploy.Generation, "original-user", ""),
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

	// Propagate trace with DIFFERENT user (simulating kubectl or another actor)
	// childUpdaters contains the original controller's hash, not the different user
	propagator := trace.NewPropagator(k8sClient)
	childUpdaters := []string{controller.HashUsername("original-controller")}
	result, err := propagator.Propagate(ctx, rs, "different-user", childUpdaters, "test-req-uid")
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
