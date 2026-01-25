package approval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestActionApplier_ApplyApproval(t *testing.T) {
	tests := []struct {
		name           string
		existingAnnots map[string]string
		child          ChildRef
		mode           string
		wantMode       string
	}{
		{
			name:           "new approval with mode once",
			existingAnnots: nil,
			child:          ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			mode:           ModeOnce,
			wantMode:       ModeOnce,
		},
		{
			name:           "new approval with mode generation",
			existingAnnots: nil,
			child:          ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			mode:           ModeGeneration,
			wantMode:       ModeGeneration,
		},
		{
			name:           "new approval with mode always",
			existingAnnots: nil,
			child:          ChildRef{APIVersion: "v1", Kind: "Secret", Name: "test-secret"},
			mode:           ModeAlways,
			wantMode:       ModeAlways,
		},
		{
			name:           "update existing approval",
			existingAnnots: map[string]string{ApprovalsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","name":"test-cm","mode":"once","generation":1}]`},
			child:          ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			mode:           ModeGeneration,
			wantMode:       ModeGeneration,
		},
		{
			name:           "default mode is once",
			existingAnnots: nil,
			child:          ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"},
			mode:           "",
			wantMode:       ModeOnce,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := createTestParent(5, tt.existingAnnots)
			fakeClient := fake.NewClientBuilder().WithObjects(parent).Build()

			applier := NewActionApplier(fakeClient)
			parentRef := ObjectRef{
				APIVersion: "example.com/v1alpha1",
				Kind:       "TestParent",
				Namespace:  "default",
				Name:       "test-parent",
			}

			err := applier.ApplyApproval(context.Background(), parentRef, tt.child, tt.mode)
			require.NoError(t, err)

			// Verify the approval was added
			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(parent.GroupVersionKind())
			err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(parent), updated)
			require.NoError(t, err)

			annotations := updated.GetAnnotations()
			require.NotNil(t, annotations)
			require.NotEmpty(t, annotations[ApprovalsAnnotation])

			approvals, err := ParseApprovals(annotations[ApprovalsAnnotation])
			require.NoError(t, err)

			// Find the approval for our child
			var found *Approval
			for i := range approvals {
				if approvals[i].Matches(tt.child) {
					found = &approvals[i]
					break
				}
			}
			require.NotNil(t, found, "approval not found for child")
			assert.Equal(t, tt.wantMode, found.Mode)
			if tt.wantMode != ModeAlways {
				assert.Equal(t, int64(5), found.Generation)
			}
		})
	}
}

func TestActionApplier_ApplyRejection(t *testing.T) {
	parent := createTestParent(3, nil)
	fakeClient := fake.NewClientBuilder().WithObjects(parent).Build()

	applier := NewActionApplier(fakeClient)
	parentRef := ObjectRef{
		APIVersion: "example.com/v1alpha1",
		Kind:       "TestParent",
		Namespace:  "default",
		Name:       "test-parent",
	}
	child := ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "test-cm"}

	err := applier.ApplyRejection(context.Background(), parentRef, child, "security review required")
	require.NoError(t, err)

	// Verify the rejection was added
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(parent.GroupVersionKind())
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(parent), updated)
	require.NoError(t, err)

	annotations := updated.GetAnnotations()
	require.NotNil(t, annotations)
	require.NotEmpty(t, annotations[RejectionsAnnotation])

	rejections, err := ParseRejections(annotations[RejectionsAnnotation])
	require.NoError(t, err)
	require.Len(t, rejections, 1)
	assert.Equal(t, "v1", rejections[0].APIVersion)
	assert.Equal(t, "ConfigMap", rejections[0].Kind)
	assert.Equal(t, "test-cm", rejections[0].Name)
	assert.Equal(t, "security review required", rejections[0].Reason)
	assert.Equal(t, int64(3), rejections[0].Generation)
}

func TestActionApplier_ApplySnooze(t *testing.T) {
	parent := createTestParent(1, nil)
	fakeClient := fake.NewClientBuilder().WithObjects(parent).Build()

	applier := NewActionApplier(fakeClient)
	parentRef := ObjectRef{
		APIVersion: "example.com/v1alpha1",
		Kind:       "TestParent",
		Namespace:  "default",
		Name:       "test-parent",
	}

	before := time.Now()
	err := applier.ApplySnooze(context.Background(), parentRef, 1*time.Hour, "admin@example.com", "deploying hotfix")
	require.NoError(t, err)
	after := time.Now()

	// Verify the snooze was set
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(parent.GroupVersionKind())
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(parent), updated)
	require.NoError(t, err)

	annotations := updated.GetAnnotations()
	require.NotNil(t, annotations)
	snoozeStr := annotations[SnoozeAnnotation]
	require.NotEmpty(t, snoozeStr)

	// Parse the structured snooze
	snooze, err := ParseSnooze(snoozeStr)
	require.NoError(t, err)
	require.NotNil(t, snooze)

	// Verify fields
	assert.Equal(t, "admin@example.com", snooze.User)
	assert.Equal(t, "deploying hotfix", snooze.Message)

	// Snooze expiry should be approximately 1 hour from now
	assert.True(t, snooze.Expiry.After(before.Add(59*time.Minute)))
	assert.True(t, snooze.Expiry.Before(after.Add(61*time.Minute)))
}

func TestActionApplier_RemoveApproval(t *testing.T) {
	existingApprovals := `[{"apiVersion":"v1","kind":"ConfigMap","name":"cm1","mode":"once"},{"apiVersion":"v1","kind":"Secret","name":"s1","mode":"always"}]`
	parent := createTestParent(1, map[string]string{
		ApprovalsAnnotation: existingApprovals,
	})
	fakeClient := fake.NewClientBuilder().WithObjects(parent).Build()

	applier := NewActionApplier(fakeClient)
	parentRef := ObjectRef{
		APIVersion: "example.com/v1alpha1",
		Kind:       "TestParent",
		Namespace:  "default",
		Name:       "test-parent",
	}
	child := ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "cm1"}

	err := applier.RemoveApproval(context.Background(), parentRef, child)
	require.NoError(t, err)

	// Verify the approval was removed
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(parent.GroupVersionKind())
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(parent), updated)
	require.NoError(t, err)

	annotations := updated.GetAnnotations()
	approvals, err := ParseApprovals(annotations[ApprovalsAnnotation])
	require.NoError(t, err)
	assert.Len(t, approvals, 1)
	assert.Equal(t, "Secret", approvals[0].Kind)
	assert.Equal(t, "s1", approvals[0].Name)
}

func TestActionApplier_RemoveRejection(t *testing.T) {
	existingRejections := `[{"apiVersion":"v1","kind":"ConfigMap","name":"cm1","reason":"test"},{"apiVersion":"v1","kind":"Secret","name":"s1","reason":"test2"}]`
	parent := createTestParent(1, map[string]string{
		RejectionsAnnotation: existingRejections,
	})
	fakeClient := fake.NewClientBuilder().WithObjects(parent).Build()

	applier := NewActionApplier(fakeClient)
	parentRef := ObjectRef{
		APIVersion: "example.com/v1alpha1",
		Kind:       "TestParent",
		Namespace:  "default",
		Name:       "test-parent",
	}
	child := ChildRef{APIVersion: "v1", Kind: "ConfigMap", Name: "cm1"}

	err := applier.RemoveRejection(context.Background(), parentRef, child)
	require.NoError(t, err)

	// Verify the rejection was removed
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(parent.GroupVersionKind())
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(parent), updated)
	require.NoError(t, err)

	annotations := updated.GetAnnotations()
	rejections, err := ParseRejections(annotations[RejectionsAnnotation])
	require.NoError(t, err)
	assert.Len(t, rejections, 1)
	assert.Equal(t, "Secret", rejections[0].Kind)
}

func TestActionApplier_ClearSnooze(t *testing.T) {
	parent := createTestParent(1, map[string]string{
		SnoozeAnnotation: "2099-01-01T00:00:00Z",
	})
	fakeClient := fake.NewClientBuilder().WithObjects(parent).Build()

	applier := NewActionApplier(fakeClient)
	parentRef := ObjectRef{
		APIVersion: "example.com/v1alpha1",
		Kind:       "TestParent",
		Namespace:  "default",
		Name:       "test-parent",
	}

	err := applier.ClearSnooze(context.Background(), parentRef)
	require.NoError(t, err)

	// Verify the snooze was cleared
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(parent.GroupVersionKind())
	err = fakeClient.Get(context.Background(), client.ObjectKeyFromObject(parent), updated)
	require.NoError(t, err)

	annotations := updated.GetAnnotations()
	assert.Empty(t, annotations[SnoozeAnnotation])
}

func TestActionApplier_FetchObjectNotFound(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()

	applier := NewActionApplier(fakeClient)
	parentRef := ObjectRef{
		APIVersion: "example.com/v1alpha1",
		Kind:       "TestParent",
		Namespace:  "default",
		Name:       "nonexistent",
	}

	err := applier.ApplyApproval(context.Background(), parentRef, ChildRef{}, ModeOnce)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to fetch parent")
}

func createTestParent(generation int64, annotations map[string]string) *unstructured.Unstructured {
	parent := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "example.com/v1alpha1",
			"kind":       "TestParent",
			"metadata": map[string]interface{}{
				"name":       "test-parent",
				"namespace":  "default",
				"generation": generation,
			},
			"spec": map[string]interface{}{},
		},
	}
	if annotations != nil {
		parent.SetAnnotations(annotations)
	}
	return parent
}

// Ensure the fake client scheme includes unstructured
func init() {
	_ = runtime.NewScheme()
}
