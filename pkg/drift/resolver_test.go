package drift

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kausality-io/kausality/pkg/controller"
)

func TestFindControllerOwnerRef(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name     string
		refs     []metav1.OwnerReference
		wantName string
		wantNil  bool
	}{
		{
			name:    "no owner refs",
			refs:    nil,
			wantNil: true,
		},
		{
			name: "owner ref without controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "test",
				},
			},
			wantNil: true,
		},
		{
			name: "owner ref with controller=false",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "test",
					Controller: &falseVal,
				},
			},
			wantNil: true,
		},
		{
			name: "owner ref with controller=true",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "controller-owner",
					Controller: &trueVal,
				},
			},
			wantName: "controller-owner",
		},
		{
			name: "multiple refs - picks controller",
			refs: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "non-controller",
					Controller: &falseVal,
				},
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "controller-owner",
					Controller: &trueVal,
				},
			},
			wantName: "controller-owner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findControllerOwnerRef(tt.refs)
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.wantName, got.Name)
		})
	}
}

func TestExtractParentState(t *testing.T) {
	trueVal := true
	ownerRef := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "parent-deploy",
		Controller: &trueVal,
	}

	tests := []struct {
		name      string
		parent    *unstructured.Unstructured
		wantGen   int64
		wantObsG  int64
		wantHasOG bool
		wantDel   bool
		wantInit  bool
		wantConds int
	}{
		{
			name: "minimal parent",
			parent: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata": map[string]interface{}{
						"name":       "parent-deploy",
						"namespace":  "default",
						"generation": int64(5),
					},
				},
			},
			wantGen:   5,
			wantHasOG: false,
		},
		{
			name: "parent with observedGeneration",
			parent: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata": map[string]interface{}{
						"name":       "parent-deploy",
						"namespace":  "default",
						"generation": int64(10),
					},
					"status": map[string]interface{}{
						"observedGeneration": int64(9),
					},
				},
			},
			wantGen:   10,
			wantObsG:  9,
			wantHasOG: true,
		},
		{
			name: "parent with conditions",
			parent: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata": map[string]interface{}{
						"name":       "parent-deploy",
						"namespace":  "default",
						"generation": int64(3),
					},
					"status": map[string]interface{}{
						"observedGeneration": int64(3),
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Ready",
								"status": "True",
							},
							map[string]interface{}{
								"type":   "Progressing",
								"status": "True",
							},
						},
					},
				},
			},
			wantGen:   3,
			wantObsG:  3,
			wantHasOG: true,
			wantConds: 2,
		},
		{
			name: "parent with phase=initialized annotation",
			parent: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata": map[string]interface{}{
						"name":       "parent-deploy",
						"namespace":  "default",
						"generation": int64(1),
						"annotations": map[string]interface{}{
							controller.PhaseAnnotation: controller.PhaseValueInitialized,
						},
					},
				},
			},
			wantGen:  1,
			wantInit: true,
		},
		{
			name: "parent being deleted",
			parent: func() *unstructured.Unstructured {
				u := &unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "apps/v1",
						"kind":       "Deployment",
						"metadata": map[string]interface{}{
							"name":              "parent-deploy",
							"namespace":         "default",
							"generation":        int64(5),
							"deletionTimestamp": time.Now().Format(time.RFC3339),
						},
					},
				}
				return u
			}(),
			wantGen: 5,
			wantDel: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := extractParentState(tt.parent, ownerRef)
			assert.Equal(t, tt.wantGen, state.Generation, "Generation")
			assert.Equal(t, tt.wantObsG, state.ObservedGeneration, "ObservedGeneration")
			assert.Equal(t, tt.wantHasOG, state.HasObservedGeneration, "HasObservedGeneration")
			assert.Equal(t, tt.wantDel, state.DeletionTimestamp != nil, "DeletionTimestamp set")
			assert.Equal(t, tt.wantInit, state.IsInitialized, "IsInitialized")
			assert.Len(t, state.Conditions, tt.wantConds, "Conditions")
		})
	}
}

func TestExtractConditions(t *testing.T) {
	tests := []struct {
		name      string
		status    map[string]interface{}
		wantCount int
		wantTypes []string
	}{
		{
			name:      "no conditions",
			status:    map[string]interface{}{},
			wantCount: 0,
		},
		{
			name: "empty conditions",
			status: map[string]interface{}{
				"conditions": []interface{}{},
			},
			wantCount: 0,
		},
		{
			name: "single condition",
			status: map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":    "Ready",
						"status":  "True",
						"reason":  "AllGood",
						"message": "Everything is ready",
					},
				},
			},
			wantCount: 1,
			wantTypes: []string{"Ready"},
		},
		{
			name: "multiple conditions",
			status: map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Ready",
						"status": "True",
					},
					map[string]interface{}{
						"type":   "Initialized",
						"status": "True",
					},
					map[string]interface{}{
						"type":   "Available",
						"status": "False",
					},
				},
			},
			wantCount: 3,
			wantTypes: []string{"Ready", "Initialized", "Available"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conditions := ExtractConditions(tt.status)
			assert.Len(t, conditions, tt.wantCount)
			for i, wantType := range tt.wantTypes {
				if i < len(conditions) {
					assert.Equal(t, wantType, conditions[i].Type, "conditions[%d].Type", i)
				}
			}
		})
	}
}

func TestParentRefFromOwnerRef(t *testing.T) {
	trueVal := true
	ref := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "test-deploy",
		Controller: &trueVal,
	}

	parentRef := ParentRefFromOwnerRef(ref, "my-namespace")

	want := ParentRef{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "test-deploy",
		Namespace:  "my-namespace",
	}
	if diff := cmp.Diff(want, parentRef); diff != "" {
		t.Errorf("ParentRefFromOwnerRef() mismatch (-want +got):\n%s", diff)
	}
}

func TestExtractConditionObservedGeneration(t *testing.T) {
	tests := []struct {
		name      string
		status    map[string]interface{}
		wantObsG  int64
		wantFound bool
	}{
		{
			name:      "no conditions",
			status:    map[string]interface{}{},
			wantObsG:  0,
			wantFound: false,
		},
		{
			name: "empty conditions",
			status: map[string]interface{}{
				"conditions": []interface{}{},
			},
			wantObsG:  0,
			wantFound: false,
		},
		{
			name: "conditions without observedGeneration",
			status: map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Synced",
						"status": "True",
					},
					map[string]interface{}{
						"type":   "Ready",
						"status": "True",
					},
				},
			},
			wantObsG:  0,
			wantFound: false,
		},
		{
			name: "Synced condition with observedGeneration",
			status: map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":               "Synced",
						"status":             "True",
						"observedGeneration": int64(5),
					},
				},
			},
			wantObsG:  5,
			wantFound: true,
		},
		{
			name: "Ready condition with observedGeneration (no Synced)",
			status: map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":               "Ready",
						"status":             "True",
						"observedGeneration": int64(7),
					},
				},
			},
			wantObsG:  7,
			wantFound: true,
		},
		{
			name: "both Synced and Ready - prefers Synced",
			status: map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":               "Ready",
						"status":             "True",
						"observedGeneration": int64(3),
					},
					map[string]interface{}{
						"type":               "Synced",
						"status":             "True",
						"observedGeneration": int64(5),
					},
				},
			},
			wantObsG:  5,
			wantFound: true,
		},
		{
			name: "Synced without observedGeneration falls back to Ready",
			status: map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":   "Synced",
						"status": "True",
					},
					map[string]interface{}{
						"type":               "Ready",
						"status":             "True",
						"observedGeneration": int64(9),
					},
				},
			},
			wantObsG:  9,
			wantFound: true,
		},
		{
			name: "other condition types are ignored",
			status: map[string]interface{}{
				"conditions": []interface{}{
					map[string]interface{}{
						"type":               "Progressing",
						"status":             "True",
						"observedGeneration": int64(10),
					},
				},
			},
			wantObsG:  0,
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obsG, found := ExtractConditionObservedGeneration(tt.status)
			assert.Equal(t, tt.wantObsG, obsG, "observedGeneration")
			assert.Equal(t, tt.wantFound, found, "found")
		})
	}
}

func TestExtractParentState_CrossplaneConditions(t *testing.T) {
	trueVal := true
	ownerRef := metav1.OwnerReference{
		APIVersion: "nop.crossplane.io/v1alpha1",
		Kind:       "NopResource",
		Name:       "parent-nop",
		Controller: &trueVal,
	}

	tests := []struct {
		name      string
		parent    *unstructured.Unstructured
		wantObsG  int64
		wantHasOG bool
	}{
		{
			name: "top-level observedGeneration takes precedence",
			parent: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]interface{}{
						"name":       "parent-nop",
						"namespace":  "default",
						"generation": int64(5),
					},
					"status": map[string]interface{}{
						"observedGeneration": int64(5),
						"conditions": []interface{}{
							map[string]interface{}{
								"type":               "Synced",
								"status":             "True",
								"observedGeneration": int64(3),
							},
						},
					},
				},
			},
			wantObsG:  5,
			wantHasOG: true,
		},
		{
			name: "Crossplane-style: observedGeneration only in Synced condition",
			parent: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]interface{}{
						"name":       "parent-nop",
						"namespace":  "default",
						"generation": int64(7),
					},
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":               "Synced",
								"status":             "True",
								"observedGeneration": int64(7),
							},
							map[string]interface{}{
								"type":   "Ready",
								"status": "True",
							},
						},
					},
				},
			},
			wantObsG:  7,
			wantHasOG: true,
		},
		{
			name: "Crossplane-style: observedGeneration only in Ready condition",
			parent: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]interface{}{
						"name":       "parent-nop",
						"namespace":  "default",
						"generation": int64(4),
					},
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":               "Ready",
								"status":             "True",
								"observedGeneration": int64(4),
							},
						},
					},
				},
			},
			wantObsG:  4,
			wantHasOG: true,
		},
		{
			name: "no observedGeneration anywhere",
			parent: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "nop.crossplane.io/v1alpha1",
					"kind":       "NopResource",
					"metadata": map[string]interface{}{
						"name":       "parent-nop",
						"namespace":  "default",
						"generation": int64(2),
					},
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Synced",
								"status": "True",
							},
						},
					},
				},
			},
			wantObsG:  0,
			wantHasOG: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := extractParentState(tt.parent, ownerRef)
			assert.Equal(t, tt.wantObsG, state.ObservedGeneration, "ObservedGeneration")
			assert.Equal(t, tt.wantHasOG, state.HasObservedGeneration, "HasObservedGeneration")
		})
	}
}
