//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kadmission "github.com/kausality-io/kausality/pkg/admission"
	"github.com/kausality-io/kausality/pkg/approval"
	"github.com/kausality-io/kausality/pkg/config"
)

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

func TestEnforceMode_NamespaceList(t *testing.T) {
	ctx := context.Background()

	// Create a namespace with a unique name
	testCounter++
	nsName := fmt.Sprintf("ns-list-test-%d", testCounter)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: nsName},
	}
	require.NoError(t, k8sClient.Create(ctx, ns))
	defer k8sClient.Delete(ctx, ns)

	// Create parent deployment in the new namespace
	deploy := createDeploymentInNamespace(t, ctx, "ns-list-deploy", nsName)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerInNamespace(t, ctx, "ns-list-rs", nsName, deploy)

	// Set parent as ready (gen == obsGen) - drift scenario
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))

	// Re-fetch to get managedFields
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler with enforce mode for SPECIFIC NAMESPACES
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog, // Default is log
				Overrides: []config.DriftDetectionOverride{
					{
						APIGroups:  []string{"apps"},
						Resources:  []string{"replicasets"},
						Namespaces: []string{nsName}, // Only enforce in this namespace
						Mode:       config.ModeEnforce,
					},
				},
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
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
			UID:       types.UID("ns-list-uid"),
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

	// Should be DENIED - namespace is in the enforce list
	assert.False(t, resp.Allowed, "expected allowed=false for namespace in enforce list")
}

func TestEnforceMode_NamespaceList_NotInList(t *testing.T) {
	ctx := context.Background()

	// Create a namespace that is NOT in the enforce list
	testCounter++
	nsName := fmt.Sprintf("ns-list-excluded-%d", testCounter)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: nsName},
	}
	require.NoError(t, k8sClient.Create(ctx, ns))
	defer k8sClient.Delete(ctx, ns)

	// Create parent deployment in the new namespace
	deploy := createDeploymentInNamespace(t, ctx, "ns-list-excluded-deploy", nsName)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerInNamespace(t, ctx, "ns-list-excluded-rs", nsName, deploy)

	// Set parent as ready (gen == obsGen) - drift scenario
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))

	// Re-fetch to get managedFields
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler with enforce mode for DIFFERENT namespace
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog,
				Overrides: []config.DriftDetectionOverride{
					{
						APIGroups:  []string{"apps"},
						Resources:  []string{"replicasets"},
						Namespaces: []string{"other-namespace"}, // NOT our namespace
						Mode:       config.ModeEnforce,
					},
				},
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
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
			UID:       types.UID("ns-list-excluded-uid"),
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

	// Should be ALLOWED - namespace is NOT in the enforce list, falls back to log mode
	assert.True(t, resp.Allowed, "expected allowed=true for namespace NOT in enforce list")
}

// =============================================================================
// Test: Namespace Selector - Enforce In Namespaces With Labels
// =============================================================================

func TestEnforceMode_NamespaceSelector(t *testing.T) {
	ctx := context.Background()

	// Create a namespace WITH the critical label
	testCounter++
	nsName := fmt.Sprintf("ns-selector-test-%d", testCounter)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nsName,
			Labels: map[string]string{"critical": "true"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ns))
	defer k8sClient.Delete(ctx, ns)

	// Create parent deployment
	deploy := createDeploymentInNamespace(t, ctx, "ns-selector-deploy", nsName)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerInNamespace(t, ctx, "ns-selector-rs", nsName, deploy)

	// Set parent as ready (gen == obsGen) - drift scenario
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))

	// Re-fetch to get managedFields
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler with namespace selector
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog,
				Overrides: []config.DriftDetectionOverride{
					{
						APIGroups: []string{"apps"},
						Resources: []string{"replicasets"},
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"critical": "true"},
						},
						Mode: config.ModeEnforce,
					},
				},
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
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
			UID:       types.UID("ns-selector-uid"),
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

	// Should be DENIED - namespace has the critical=true label
	assert.False(t, resp.Allowed, "expected allowed=false for namespace with matching selector")
}

func TestEnforceMode_NamespaceSelector_NoMatch(t *testing.T) {
	ctx := context.Background()

	// Create a namespace WITHOUT the critical label
	testCounter++
	nsName := fmt.Sprintf("ns-selector-nomatch-%d", testCounter)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nsName,
			Labels: map[string]string{"env": "dev"}, // No critical label
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ns))
	defer k8sClient.Delete(ctx, ns)

	// Create parent deployment
	deploy := createDeploymentInNamespace(t, ctx, "ns-selector-nomatch-deploy", nsName)

	// Create child ReplicaSet
	rs := createReplicaSetWithOwnerInNamespace(t, ctx, "ns-selector-nomatch-rs", nsName, deploy)

	// Set parent as ready (gen == obsGen) - drift scenario
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))

	// Re-fetch to get managedFields
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler with namespace selector
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog,
				Overrides: []config.DriftDetectionOverride{
					{
						APIGroups: []string{"apps"},
						Resources: []string{"replicasets"},
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"critical": "true"},
						},
						Mode: config.ModeEnforce,
					},
				},
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
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
			UID:       types.UID("ns-selector-nomatch-uid"),
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

	// Should be ALLOWED - namespace does NOT have the critical=true label
	assert.True(t, resp.Allowed, "expected allowed=true for namespace without matching selector")
}

// =============================================================================
// Test: Object Selector - Enforce For Objects With Labels
// =============================================================================

func TestEnforceMode_ObjectSelector(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment WITH protected label
	deploy := createDeploymentWithLabels(t, ctx, "obj-selector-deploy", map[string]string{"protected": "true"})

	// Create child ReplicaSet with labels
	rs := createReplicaSetWithOwnerAndLabels(t, ctx, "obj-selector-rs", deploy, map[string]string{"protected": "true"})

	// Set parent as ready (gen == obsGen) - drift scenario
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))

	// Re-fetch to get managedFields
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler with object selector
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog,
				Overrides: []config.DriftDetectionOverride{
					{
						APIGroups: []string{"apps"},
						Resources: []string{"replicasets"},
						ObjectSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"protected": "true"},
						},
						Mode: config.ModeEnforce,
					},
				},
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
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
			UID:       types.UID("obj-selector-uid"),
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

	// Should be DENIED - object has the protected=true label
	assert.False(t, resp.Allowed, "expected allowed=false for object with matching selector")
}

func TestEnforceMode_ObjectSelector_NoMatch(t *testing.T) {
	ctx := context.Background()

	// Create parent deployment WITHOUT protected label
	deploy := createDeploymentWithLabels(t, ctx, "obj-selector-nomatch-deploy", map[string]string{"tier": "frontend"})

	// Create child ReplicaSet WITHOUT protected label
	rs := createReplicaSetWithOwnerAndLabels(t, ctx, "obj-selector-nomatch-rs", deploy, map[string]string{"tier": "frontend"})

	// Set parent as ready (gen == obsGen) - drift scenario
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Status.ObservedGeneration = deploy.Generation
	require.NoError(t, k8sClient.Status().Update(ctx, deploy))

	// Re-fetch to get managedFields
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))

	var controllerManager string
	for _, mf := range deploy.ManagedFields {
		if mf.Subresource == "status" {
			controllerManager = mf.Manager
			break
		}
	}

	// Create handler with object selector
	handler := kadmission.NewHandler(kadmission.Config{
		Client: k8sClient,
		Log:    logr.Discard(),
		DriftConfig: &config.Config{
			DriftDetection: config.DriftDetectionConfig{
				DefaultMode: config.ModeLog,
				Overrides: []config.DriftDetectionOverride{
					{
						APIGroups: []string{"apps"},
						Resources: []string{"replicasets"},
						ObjectSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"protected": "true"},
						},
						Mode: config.ModeEnforce,
					},
				},
			},
		},
	})

	// Re-fetch RS and set TypeMeta
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs))
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
			UID:       types.UID("obj-selector-nomatch-uid"),
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

	// Should be ALLOWED - object does NOT have the protected=true label
	assert.True(t, resp.Allowed, "expected allowed=true for object without matching selector")
}
