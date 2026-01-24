package approval

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestChecker_Check(t *testing.T) {
	checker := NewChecker()
	child := ChildRef{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "test-cm",
	}

	tests := []struct {
		name             string
		annotations      map[string]string
		parentGeneration int64
		wantApproved     bool
		wantRejected     bool
	}{
		{
			name:             "no annotations",
			annotations:      nil,
			parentGeneration: 1,
			wantApproved:     false,
			wantRejected:     false,
		},
		{
			name:             "empty annotations",
			annotations:      map[string]string{},
			parentGeneration: 1,
			wantApproved:     false,
			wantRejected:     false,
		},
		{
			name: "matching approval - mode always",
			annotations: map[string]string{
				ApprovalsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","mode":"always"}]`,
			},
			parentGeneration: 99,
			wantApproved:     true,
			wantRejected:     false,
		},
		{
			name: "matching approval - mode once valid",
			annotations: map[string]string{
				ApprovalsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","generation":5,"mode":"once"}]`,
			},
			parentGeneration: 5,
			wantApproved:     true,
			wantRejected:     false,
		},
		{
			name: "matching approval - mode once stale",
			annotations: map[string]string{
				ApprovalsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","generation":5,"mode":"once"}]`,
			},
			parentGeneration: 6,
			wantApproved:     false,
			wantRejected:     false,
		},
		{
			name: "matching approval - mode generation valid",
			annotations: map[string]string{
				ApprovalsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","generation":10,"mode":"generation"}]`,
			},
			parentGeneration: 10,
			wantApproved:     true,
			wantRejected:     false,
		},
		{
			name: "no matching approval",
			annotations: map[string]string{
				ApprovalsAnnotation: `[{"apiVersion":"v1","kind":"Secret","name":"other","mode":"always"}]`,
			},
			parentGeneration: 1,
			wantApproved:     false,
			wantRejected:     false,
		},
		{
			name: "matching rejection",
			annotations: map[string]string{
				RejectionsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","reason":"dangerous"}]`,
			},
			parentGeneration: 1,
			wantApproved:     false,
			wantRejected:     true,
		},
		{
			name: "rejection wins over approval",
			annotations: map[string]string{
				ApprovalsAnnotation:  `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","mode":"always"}]`,
				RejectionsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","reason":"nope"}]`,
			},
			parentGeneration: 1,
			wantApproved:     false,
			wantRejected:     true,
		},
		{
			name: "rejection with generation - matching",
			annotations: map[string]string{
				RejectionsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","generation":5,"reason":"bad"}]`,
			},
			parentGeneration: 5,
			wantApproved:     false,
			wantRejected:     true,
		},
		{
			name: "rejection with generation - not matching",
			annotations: map[string]string{
				RejectionsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","generation":5,"reason":"bad"}]`,
			},
			parentGeneration: 6,
			wantApproved:     false,
			wantRejected:     false,
		},
		{
			name: "invalid approvals json",
			annotations: map[string]string{
				ApprovalsAnnotation: `not valid json`,
			},
			parentGeneration: 1,
			wantApproved:     false,
			wantRejected:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata": map[string]interface{}{
						"name":        "parent",
						"namespace":   "default",
						"annotations": toInterfaceMap(tt.annotations),
					},
				},
			}

			result := checker.Check(parent, child, tt.parentGeneration)

			if result.Approved != tt.wantApproved {
				t.Errorf("Approved = %v, want %v (reason: %s)", result.Approved, tt.wantApproved, result.Reason)
			}
			if result.Rejected != tt.wantRejected {
				t.Errorf("Rejected = %v, want %v (reason: %s)", result.Rejected, tt.wantRejected, result.Reason)
			}
		})
	}
}

func TestChecker_MatchedApproval(t *testing.T) {
	checker := NewChecker()
	child := ChildRef{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "test-cm",
	}

	parent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "parent",
				"namespace": "default",
				"annotations": map[string]interface{}{
					ApprovalsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","generation":5,"mode":"once"}]`,
				},
			},
		},
	}

	result := checker.Check(parent, child, 5)

	if !result.Approved {
		t.Fatalf("expected approved")
	}
	if result.MatchedApproval == nil {
		t.Fatal("expected MatchedApproval to be set")
	}
	if result.MatchedApproval.Mode != ModeOnce {
		t.Errorf("MatchedApproval.Mode = %q, want %q", result.MatchedApproval.Mode, ModeOnce)
	}
	if result.MatchedApproval.Generation != 5 {
		t.Errorf("MatchedApproval.Generation = %d, want 5", result.MatchedApproval.Generation)
	}
}

func TestChecker_MatchedRejection(t *testing.T) {
	checker := NewChecker()
	child := ChildRef{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "test-cm",
	}

	parent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]interface{}{
				"name":      "parent",
				"namespace": "default",
				"annotations": map[string]interface{}{
					RejectionsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","reason":"too risky"}]`,
				},
			},
		},
	}

	result := checker.Check(parent, child, 1)

	if !result.Rejected {
		t.Fatalf("expected rejected")
	}
	if result.MatchedRejection == nil {
		t.Fatal("expected MatchedRejection to be set")
	}
	if result.MatchedRejection.Reason != "too risky" {
		t.Errorf("MatchedRejection.Reason = %q, want %q", result.MatchedRejection.Reason, "too risky")
	}
	if result.Reason != "too risky" {
		t.Errorf("Reason = %q, want %q", result.Reason, "too risky")
	}
}

func TestCheckFromAnnotations(t *testing.T) {
	child := ChildRef{
		APIVersion: "v1",
		Kind:       "Secret",
		Name:       "creds",
	}

	tests := []struct {
		name         string
		approvals    string
		rejections   string
		parentGen    int64
		wantApproved bool
		wantRejected bool
	}{
		{
			name:         "approved",
			approvals:    `[{"apiVersion":"v1","kind":"Secret","name":"creds","mode":"always"}]`,
			rejections:   "",
			parentGen:    1,
			wantApproved: true,
		},
		{
			name:         "rejected",
			approvals:    "",
			rejections:   `[{"apiVersion":"v1","kind":"Secret","name":"creds","reason":"nope"}]`,
			parentGen:    1,
			wantRejected: true,
		},
		{
			name:         "neither",
			approvals:    "",
			rejections:   "",
			parentGen:    1,
			wantApproved: false,
			wantRejected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CheckFromAnnotations(tt.approvals, tt.rejections, child, tt.parentGen)
			if result.Approved != tt.wantApproved {
				t.Errorf("Approved = %v, want %v", result.Approved, tt.wantApproved)
			}
			if result.Rejected != tt.wantRejected {
				t.Errorf("Rejected = %v, want %v", result.Rejected, tt.wantRejected)
			}
		})
	}
}

// toInterfaceMap converts map[string]string to map[string]interface{} for unstructured.
func toInterfaceMap(m map[string]string) map[string]interface{} {
	if m == nil {
		return nil
	}
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

// Ensure unstructured implements client.Object
var _ metav1.Object = &unstructured.Unstructured{}
