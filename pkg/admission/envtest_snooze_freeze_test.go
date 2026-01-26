//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kadmission "github.com/kausality-io/kausality/pkg/admission"
	"github.com/kausality-io/kausality/pkg/approval"
	"github.com/kausality-io/kausality/pkg/callback"
	"github.com/kausality-io/kausality/pkg/callback/v1alpha1"
	"github.com/kausality-io/kausality/pkg/config"
	"github.com/kausality-io/kausality/pkg/controller"
)

// =============================================================================
// Snooze Tests
// =============================================================================

// TestSnooze_SuppressesCallbackWhenActive verifies that when a parent has an
// active snooze annotation, drift callbacks are suppressed but drift detection
// still works (blocking is not affected).
func TestSnooze_SuppressesCallbackWhenActive(t *testing.T) {
	ctx := context.Background()

	// Create a mock webhook server to receive drift reports
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		response := v1alpha1.DriftReportResponse{Acknowledged: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create callback sender
	callbackSender, err := callback.NewSender(callback.SenderConfig{
		URL:        server.URL,
		Timeout:    5 * time.Second,
		RetryCount: 0,
		Log:        ctrl.Log.WithName("test-snooze"),
	})
	if err != nil {
		t.Fatalf("failed to create callback sender: %v", err)
	}

	// Create parent deployment with snooze annotation
	deploy := createDeployment(t, ctx, "snooze-deploy")

	// Add snooze annotation (snooze until 1 hour from now)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	snoozeUntil := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	annotations[approval.SnoozeAnnotation] = snoozeUntil
	annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "snooze-rs", deploy)

	// Set parent as ready (drift scenario: gen == obsGen)
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

	// Create handler with callback sender
	handler := kadmission.NewHandler(kadmission.Config{
		Client:         k8sClient,
		Log:            ctrl.Log.WithName("test-snooze"),
		CallbackSender: callbackSender,
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog, // Non-enforce to allow but still detect
			},
		},
	})

	// Re-fetch RS and set TypeMeta
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
			UID:       types.UID("snooze-uid"),
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

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, warnings=%v", resp.Allowed, resp.Warnings)

	// Should be allowed (non-enforce mode) and have drift warning
	if !resp.Allowed {
		t.Errorf("expected allowed=true in non-enforce mode")
	}

	// Check for drift warning (proving drift was detected)
	hasDriftWarning := false
	for _, w := range resp.Warnings {
		if containsSubstring(w, "drift") {
			hasDriftWarning = true
			break
		}
	}
	if !hasDriftWarning {
		t.Errorf("expected drift warning in response")
	}

	// Wait for async callback (if any) to complete
	time.Sleep(200 * time.Millisecond)

	// Verify NO callback was sent (snooze suppresses it)
	if callCount.Load() != 0 {
		t.Errorf("expected no drift callback when snoozed, but got %d calls", callCount.Load())
	}

	t.Log("SUCCESS: Snooze suppressed callback while drift was still detected")
}

// TestSnooze_ExpiredDoesNotSuppressCallback verifies that when a snooze
// timestamp has expired, callbacks are NOT suppressed.
func TestSnooze_ExpiredDoesNotSuppressCallback(t *testing.T) {
	ctx := context.Background()

	// Create a mock webhook server to receive drift reports
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		response := v1alpha1.DriftReportResponse{Acknowledged: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create callback sender
	callbackSender, err := callback.NewSender(callback.SenderConfig{
		URL:        server.URL,
		Timeout:    5 * time.Second,
		RetryCount: 0,
		Log:        ctrl.Log.WithName("test-expired-snooze"),
	})
	if err != nil {
		t.Fatalf("failed to create callback sender: %v", err)
	}

	// Create parent deployment with EXPIRED snooze annotation
	deploy := createDeployment(t, ctx, "expired-snooze-deploy")

	// Add expired snooze annotation (snooze until 1 hour AGO)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	expiredSnooze := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	annotations[approval.SnoozeAnnotation] = expiredSnooze
	annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "expired-snooze-rs", deploy)

	// Set parent as ready (drift scenario)
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

	// Find the controller manager
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler with callback sender
	handler := kadmission.NewHandler(kadmission.Config{
		Client:         k8sClient,
		Log:            ctrl.Log.WithName("test-expired-snooze"),
		CallbackSender: callbackSender,
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

	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, _ := json.Marshal(oldRS)
	newBytes, _ := json.Marshal(newRS)
	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("expired-snooze-uid"),
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

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v", resp.Allowed)

	// Wait for async callback
	time.Sleep(200 * time.Millisecond)

	// Verify callback WAS sent (expired snooze doesn't suppress)
	if callCount.Load() == 0 {
		t.Errorf("expected drift callback when snooze expired, but got 0 calls")
	}

	t.Log("SUCCESS: Expired snooze did not suppress callback")
}

// =============================================================================
// Freeze Tests
// =============================================================================

// TestFreeze_BlocksAllMutations verifies that when a parent has freeze=true,
// ALL child mutations are blocked, even if there's an approval.
func TestFreeze_BlocksAllMutations(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment with freeze AND approval
	deploy := createDeployment(t, ctx, "freeze-deploy")

	// Create child ReplicaSet FIRST
	rs := createReplicaSetWithOwner(t, ctx, "freeze-rs", deploy)

	// Add freeze annotation AND approval (freeze should override)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Add both freeze and approval
	approvals := []approval.Approval{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, Mode: approval.ModeAlways},
	}
	approvalsJSON, _ := approval.MarshalApprovals(approvals)

	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.FreezeAnnotation] = "true"
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

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

	// Find the controller manager
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler (enforce mode)
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    ctrl.Log.WithName("test-freeze"),
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

	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, _ := json.Marshal(oldRS)
	newBytes, _ := json.Marshal(newRS)
	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("freeze-uid"),
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

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, result=%v", resp.Allowed, resp.Result)

	// Should be denied (freeze blocks ALL mutations)
	if resp.Allowed {
		t.Errorf("expected allowed=false when parent is frozen, even with approval")
	}

	// Verify error message mentions freeze
	if resp.Result == nil {
		t.Fatal("expected Result to be set")
	}
	if !containsSubstring(resp.Result.Message, "frozen") {
		t.Errorf("expected freeze message, got: %s", resp.Result.Message)
	}

	t.Log("SUCCESS: Freeze blocked mutation even with approval annotation")
}

// TestFreeze_FalseDoesNotBlock verifies that freeze=false does not block.
func TestFreeze_FalseDoesNotBlock(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment with freeze=false
	deploy := createDeployment(t, ctx, "freeze-false-deploy")

	// Create child ReplicaSet FIRST
	rs := createReplicaSetWithOwner(t, ctx, "freeze-false-rs", deploy)

	// Add freeze=false and approval
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
	annotations[approval.FreezeAnnotation] = "false" // Explicitly false
	annotations[approval.ApprovalsAnnotation] = approvalsJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

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

	// Find the controller manager
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler (enforce mode)
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    ctrl.Log.WithName("test-freeze-false"),
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

	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, _ := json.Marshal(oldRS)
	newBytes, _ := json.Marshal(newRS)
	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("freeze-false-uid"),
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

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v", resp.Allowed)

	// Should be allowed (freeze=false + approval)
	if !resp.Allowed {
		t.Errorf("expected allowed=true when freeze=false with approval")
	}

	t.Log("SUCCESS: freeze=false does not block when approval exists")
}

// TestFreeze_StructuredAnnotation_MessageInDenial verifies that the structured
// freeze annotation (with user, message, at) is parsed correctly and the
// denial message includes all the context.
func TestFreeze_StructuredAnnotation_MessageInDenial(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment
	deploy := createDeployment(t, ctx, "structured-freeze-deploy")

	// Create child ReplicaSet FIRST
	rs := createReplicaSetWithOwner(t, ctx, "structured-freeze-rs", deploy)

	// Add structured freeze annotation
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	// Create structured freeze JSON
	freezeTime := time.Now().Add(-10 * time.Minute).UTC()
	freezeJSON := fmt.Sprintf(`{"user":"admin@example.com","message":"investigating incident #123","at":%q}`, freezeTime.Format(time.RFC3339))

	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.FreezeAnnotation] = freezeJSON
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

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

	// Find the controller manager
	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler (enforce mode)
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    ctrl.Log.WithName("test-structured-freeze"),
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

	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, _ := json.Marshal(oldRS)
	newBytes, _ := json.Marshal(newRS)
	optionsJSON := fmt.Sprintf(`{"fieldManager":%q}`, controllerManager)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("structured-freeze-uid"),
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

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, result=%v", resp.Allowed, resp.Result)

	// Should be denied
	if resp.Allowed {
		t.Errorf("expected allowed=false when parent is frozen")
	}

	// Verify error message contains structured freeze info
	if resp.Result == nil {
		t.Fatal("expected Result to be set")
	}

	msg := resp.Result.Message
	t.Logf("Denial message: %s", msg)

	// Check that the message contains key information (but NOT the user for privacy)
	if !containsSubstring(msg, "frozen") {
		t.Errorf("expected message to contain 'frozen', got: %s", msg)
	}
	if !containsSubstring(msg, "investigating incident #123") {
		t.Errorf("expected message to contain reason 'investigating incident #123', got: %s", msg)
	}
	// User should NOT be in the public error message (privacy)
	if containsSubstring(msg, "admin@example.com") {
		t.Errorf("user should NOT be exposed in denial message, got: %s", msg)
	}

	t.Log("SUCCESS: Structured freeze annotation provides detailed denial message without exposing user")
}

// TestSnooze_StructuredAnnotation verifies that the structured snooze annotation
// (with expiry, user, message) is parsed correctly and behaves like the legacy format.
func TestSnooze_StructuredAnnotation(t *testing.T) {
	ctx := context.Background()

	// Create a mock webhook server to receive drift reports
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		response := v1alpha1.DriftReportResponse{Acknowledged: true}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	// Create callback sender
	callbackSender, err := callback.NewSender(callback.SenderConfig{
		URL:        server.URL,
		Timeout:    5 * time.Second,
		RetryCount: 0,
		Log:        ctrl.Log.WithName("test-structured-snooze"),
	})
	if err != nil {
		t.Fatalf("failed to create callback sender: %v", err)
	}

	// Create parent deployment with structured snooze annotation
	deploy := createDeployment(t, ctx, "structured-snooze-deploy")

	// Add structured snooze annotation (snooze until 1 hour from now)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	snoozeExpiry := time.Now().Add(1 * time.Hour).UTC()
	snoozeJSON := fmt.Sprintf(`{"expiry":%q,"user":"ops@example.com","message":"deploying hotfix v1.2.3"}`, snoozeExpiry.Format(time.RFC3339))
	annotations[approval.SnoozeAnnotation] = snoozeJSON
	annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update deployment: %v", err)
	}

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "structured-snooze-rs", deploy)

	// Set parent as ready (drift scenario: gen == obsGen)
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

	// Create handler with callback sender
	handler := kadmission.NewHandler(kadmission.Config{
		Client:         k8sClient,
		Log:            ctrl.Log.WithName("test-structured-snooze"),
		CallbackSender: callbackSender,
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog, // Non-enforce to allow but still detect
			},
		},
	})

	// Re-fetch RS and set TypeMeta
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
			UID:       types.UID("structured-snooze-uid"),
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

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, warnings=%v", resp.Allowed, resp.Warnings)

	// Should be allowed (non-enforce mode) and have drift warning
	if !resp.Allowed {
		t.Errorf("expected allowed=true in non-enforce mode")
	}

	// Check for drift warning (proving drift was detected)
	hasDriftWarning := false
	for _, w := range resp.Warnings {
		if containsSubstring(w, "drift") {
			hasDriftWarning = true
			break
		}
	}
	if !hasDriftWarning {
		t.Errorf("expected drift warning in response")
	}

	// Wait for async callback (if any) to complete
	time.Sleep(200 * time.Millisecond)

	// Verify NO callback was sent (snooze suppresses it)
	if callCount.Load() != 0 {
		t.Errorf("expected no drift callback when snoozed with structured annotation, but got %d calls", callCount.Load())
	}

	t.Log("SUCCESS: Structured snooze annotation suppressed callback while drift was still detected")
}

// =============================================================================
// Test: Freeze during Deletion Phase
// =============================================================================

// TestFreeze_AllowedDuringDeletion verifies that when a parent is being deleted
// (has deletionTimestamp), mutations are allowed even if freeze is set.
// This is critical for cleanup - controllers must be able to delete children.
func TestFreeze_AllowedDuringDeletion(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment with a finalizer (so delete sets deletionTimestamp)
	deploy := createDeployment(t, ctx, "freeze-delete-deploy")

	// Add finalizer to prevent immediate deletion
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Finalizers = append(deploy.Finalizers, "test.kausality.io/block-deletion")
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to add finalizer: %v", err)
	}

	// Create child ReplicaSet FIRST
	rs := createReplicaSetWithOwner(t, ctx, "freeze-delete-rs", deploy)

	// Add freeze annotation
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	annotations := deploy.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[approval.FreezeAnnotation] = "true"
	deploy.SetAnnotations(annotations)
	if err := k8sClient.Update(ctx, deploy); err != nil {
		t.Fatalf("failed to add freeze annotation: %v", err)
	}

	// Set parent as ready (so freeze would normally block)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	deploy.Status.ObservedGeneration = deploy.Generation
	if err := k8sClient.Status().Update(ctx, deploy); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	// Delete the deployment (sets deletionTimestamp but doesn't delete due to finalizer)
	if err := k8sClient.Delete(ctx, deploy); err != nil {
		t.Fatalf("failed to delete deployment: %v", err)
	}

	// Re-fetch to verify deletionTimestamp is set
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
	if deploy.DeletionTimestamp == nil {
		t.Fatal("expected deletionTimestamp to be set")
	}
	t.Logf("Deployment has deletionTimestamp: %v", deploy.DeletionTimestamp)

	// Verify freeze is still set
	if deploy.Annotations[approval.FreezeAnnotation] != "true" {
		t.Fatal("freeze annotation should still be set")
	}

	// Create handler (enforce mode to make test meaningful)
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    ctrl.Log.WithName("test-freeze-delete"),
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

	oldRS := rs.DeepCopy()
	newRS := rs.DeepCopy()
	newReplicas := int32(3)
	newRS.Spec.Replicas = &newReplicas

	oldBytes, _ := json.Marshal(oldRS)
	newBytes, _ := json.Marshal(newRS)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       types.UID("freeze-delete-uid"),
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
			Namespace: rs.Namespace,
			Name:      rs.Name,
			Object:    runtime.RawExtension{Raw: newBytes},
			OldObject: runtime.RawExtension{Raw: oldBytes},
			UserInfo:  authenticationv1.UserInfo{Username: "controller"},
		},
	}

	// Handle the request
	resp := handler.Handle(ctx, req)

	t.Logf("Response: allowed=%v, result=%v", resp.Allowed, resp.Result)

	// Should be ALLOWED - freeze doesn't block during deletion
	if !resp.Allowed {
		t.Errorf("expected allowed=true during deletion even with freeze, got false")
		if resp.Result != nil {
			t.Errorf("denial reason: %s", resp.Result.Message)
		}
	}

	t.Log("SUCCESS: Freeze does NOT block mutations when parent is being deleted")

	// Cleanup: remove finalizer to allow deletion
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err == nil {
		deploy.Finalizers = nil
		_ = k8sClient.Update(ctx, deploy)
	}
}
