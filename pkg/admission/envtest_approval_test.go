//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/kausality-io/kausality/pkg/config"
)

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

// =============================================================================
// Test: Callback Webhook - Drift Report Sent on Detection
// =============================================================================
