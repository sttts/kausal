//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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
)

func TestCallback_DriftReportSentOnDetection(t *testing.T) {
	ctx := context.Background()

	// Create a mock webhook server to receive drift reports
	var receivedReports []*v1alpha1.DriftReport
	var callCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)

		var report v1alpha1.DriftReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Logf("Failed to decode drift report: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		receivedReports = append(receivedReports, &report)

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
		Log:        ctrl.Log.WithName("test-callback"),
	})
	if err != nil {
		t.Fatalf("failed to create callback sender: %v", err)
	}

	// Create parent deployment
	deploy := createDeployment(t, ctx, "callback-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "callback-rs", deploy)

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

	// Create handler with callback sender
	handler := kadmission.NewHandler(kadmission.Config{
		Client:         k8sClient,
		Log:            ctrl.Log.WithName("test-callback"),
		CallbackSender: callbackSender,
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog, // Non-enforce mode to allow request
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
			UID:       types.UID("callback-uid"),
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

	// Should be allowed (non-enforce mode)
	if !resp.Allowed {
		t.Errorf("expected allowed=true in non-enforce mode")
	}

	// Wait for async callback to complete
	time.Sleep(200 * time.Millisecond)

	// Verify drift report was sent
	if callCount.Load() == 0 {
		t.Errorf("expected drift report to be sent, but no calls received")
	}

	if len(receivedReports) == 0 {
		t.Fatalf("expected to receive drift report")
	}

	report := receivedReports[0]
	t.Logf("Received drift report: phase=%s, id=%s", report.Spec.Phase, report.Spec.ID)

	// Verify report contents
	if report.Spec.Phase != v1alpha1.DriftReportPhaseDetected {
		t.Errorf("expected phase=Detected, got %s", report.Spec.Phase)
	}
	if report.Spec.ID == "" {
		t.Errorf("expected non-empty ID")
	}
	if report.Spec.Parent.Kind != "Deployment" {
		t.Errorf("expected parent kind=Deployment, got %s", report.Spec.Parent.Kind)
	}
	if report.Spec.Child.Kind != "ReplicaSet" {
		t.Errorf("expected child kind=ReplicaSet, got %s", report.Spec.Child.Kind)
	}
	if report.Spec.Request.Operation != "UPDATE" {
		t.Errorf("expected operation=UPDATE, got %s", report.Spec.Request.Operation)
	}
}

// =============================================================================
// Test: Callback Webhook - Resolved Sent on Approval
// =============================================================================

func TestCallback_ResolvedSentOnApproval(t *testing.T) {
	ctx := context.Background()

	// Create a mock webhook server to receive drift reports
	var receivedReports []*v1alpha1.DriftReport
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report v1alpha1.DriftReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mu.Lock()
		receivedReports = append(receivedReports, &report)
		mu.Unlock()

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
		Log:        ctrl.Log.WithName("test-resolved"),
	})
	if err != nil {
		t.Fatalf("failed to create callback sender: %v", err)
	}

	// Create parent deployment
	deploy := createDeployment(t, ctx, "resolved-deploy")

	// Create child ReplicaSet
	rs := createReplicaSetWithOwner(t, ctx, "resolved-rs", deploy)

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

	// Create handler with callback sender
	handler := kadmission.NewHandler(kadmission.Config{
		Client:         k8sClient,
		Log:            ctrl.Log.WithName("test-resolved"),
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
			UID:       types.UID("resolved-uid"),
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
		t.Errorf("expected allowed=true with approval")
	}

	// Wait for async callback to complete
	time.Sleep(200 * time.Millisecond)

	// Verify resolved report was sent
	mu.Lock()
	reports := make([]*v1alpha1.DriftReport, len(receivedReports))
	copy(reports, receivedReports)
	mu.Unlock()

	if len(reports) == 0 {
		t.Fatalf("expected to receive drift report")
	}

	// Should receive a Resolved report (since approval was found)
	var foundResolved bool
	for _, report := range reports {
		t.Logf("Received report: phase=%s, id=%s", report.Spec.Phase, report.Spec.ID)
		if report.Spec.Phase == v1alpha1.DriftReportPhaseResolved {
			foundResolved = true
		}
	}

	if !foundResolved {
		t.Errorf("expected to receive phase=Resolved report")
	}
}

// =============================================================================
// Test: Namespace List Selector - Enforce Only In Specific Namespaces
// =============================================================================
