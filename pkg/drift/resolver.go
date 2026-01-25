package drift

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"

	"github.com/kausality-io/kausality/pkg/controller"
)

// ParentResolver resolves the controller parent of a Kubernetes object.
type ParentResolver struct {
	client client.Client
}

// NewParentResolver creates a new ParentResolver.
func NewParentResolver(c client.Client) *ParentResolver {
	return &ParentResolver{client: c}
}

// ResolveParent finds and fetches the controller parent of the given object.
// It returns nil if no controller owner reference is found.
func (r *ParentResolver) ResolveParent(ctx context.Context, obj client.Object) (*ParentState, error) {
	// Find controller owner reference
	ownerRef := findControllerOwnerRef(obj.GetOwnerReferences())
	if ownerRef == nil {
		return nil, nil
	}

	// Parse API version to get group/version
	gv, err := schema.ParseGroupVersion(ownerRef.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid API version %q: %w", ownerRef.APIVersion, err)
	}

	// Fetch the parent object
	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(gv.WithKind(ownerRef.Kind))

	// Use the same namespace as the child for namespaced resources
	parentKey := client.ObjectKey{
		Namespace: obj.GetNamespace(),
		Name:      ownerRef.Name,
	}

	if err := r.client.Get(ctx, parentKey, parent); err != nil {
		return nil, fmt.Errorf("failed to get parent %s/%s: %w", ownerRef.Kind, ownerRef.Name, err)
	}

	return extractParentState(parent, *ownerRef), nil
}

// findControllerOwnerRef finds the owner reference with controller: true.
func findControllerOwnerRef(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// extractParentState extracts drift-relevant state from an unstructured parent object.
func extractParentState(parent *unstructured.Unstructured, ownerRef metav1.OwnerReference) *ParentState {
	state := &ParentState{
		Ref: ParentRef{
			APIVersion: ownerRef.APIVersion,
			Kind:       ownerRef.Kind,
			Namespace:  parent.GetNamespace(),
			Name:       ownerRef.Name,
		},
		Generation: parent.GetGeneration(),
	}

	// Extract status.observedGeneration, falling back to condition observedGeneration
	if status, ok, _ := unstructured.NestedMap(parent.Object, "status"); ok {
		if obsGen, ok, _ := unstructured.NestedInt64(status, "observedGeneration"); ok {
			state.ObservedGeneration = obsGen
			state.HasObservedGeneration = true
		}

		// Extract conditions for lifecycle detection
		state.Conditions = extractConditions(status)

		// Fallback: if no status.observedGeneration, check Synced/Ready conditions
		// This supports Crossplane which stores observedGeneration in conditions
		if !state.HasObservedGeneration {
			state.ObservedGeneration, state.HasObservedGeneration = extractConditionObservedGeneration(status)
		}
	}

	// Find the controller manager from managedFields
	state.ControllerManager = findControllerManager(parent.GetManagedFields())

	// Check for deletion timestamp
	if parent.GetDeletionTimestamp() != nil {
		state.DeletionTimestamp = parent.GetDeletionTimestamp()
	}

	// Check annotations
	if annotations := parent.GetAnnotations(); annotations != nil {
		if annotations["kausality.io/initialized"] == "true" {
			state.IsInitialized = true
		}

		// Extract controller hashes from kausality.io/controllers annotation
		if controllers := annotations[controller.ControllersAnnotation]; controllers != "" {
			state.Controllers = parseControllerHashes(controllers)
		}
	}

	return state
}

// parseControllerHashes splits a comma-separated hash string.
func parseControllerHashes(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// extractConditionObservedGeneration extracts observedGeneration from Synced or Ready conditions.
// Returns the observedGeneration and whether it was found.
// Prefers Synced condition, falls back to Ready.
func extractConditionObservedGeneration(status map[string]interface{}) (int64, bool) {
	conditionsRaw, ok, _ := unstructured.NestedSlice(status, "conditions")
	if !ok {
		return 0, false
	}

	// First pass: look for Synced condition
	for _, c := range conditionsRaw {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(condMap, "type")
		if condType == "Synced" {
			if obsGen, ok, _ := unstructured.NestedInt64(condMap, "observedGeneration"); ok {
				return obsGen, true
			}
		}
	}

	// Second pass: fall back to Ready condition
	for _, c := range conditionsRaw {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(condMap, "type")
		if condType == "Ready" {
			if obsGen, ok, _ := unstructured.NestedInt64(condMap, "observedGeneration"); ok {
				return obsGen, true
			}
		}
	}

	return 0, false
}

// extractConditions extracts metav1.Condition list from status map.
func extractConditions(status map[string]interface{}) []metav1.Condition {
	conditionsRaw, ok, _ := unstructured.NestedSlice(status, "conditions")
	if !ok {
		return nil
	}

	var conditions []metav1.Condition
	for _, c := range conditionsRaw {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}

		cond := metav1.Condition{}
		if t, ok, _ := unstructured.NestedString(condMap, "type"); ok {
			cond.Type = t
		}
		if s, ok, _ := unstructured.NestedString(condMap, "status"); ok {
			cond.Status = metav1.ConditionStatus(s)
		}
		if r, ok, _ := unstructured.NestedString(condMap, "reason"); ok {
			cond.Reason = r
		}
		if m, ok, _ := unstructured.NestedString(condMap, "message"); ok {
			cond.Message = m
		}

		conditions = append(conditions, cond)
	}

	return conditions
}

// ParentRefFromOwnerRef converts an OwnerReference to a ParentRef.
func ParentRefFromOwnerRef(ref metav1.OwnerReference, namespace string) ParentRef {
	return ParentRef{
		APIVersion: ref.APIVersion,
		Kind:       ref.Kind,
		Namespace:  namespace,
		Name:       ref.Name,
	}
}

// findControllerManager finds the manager that owns status.observedGeneration.
// This identifies the controller that reconciles the object.
// It checks both status.observedGeneration and status.conditions[].observedGeneration
// to support Crossplane which stores observedGeneration in conditions.
func findControllerManager(managedFields []metav1.ManagedFieldsEntry) string {
	// Path to status.observedGeneration
	obsGenPath := fieldpath.MakePathOrDie("status", "observedGeneration")

	// Paths to condition observedGeneration (Synced and Ready)
	// These are structured as: status.conditions[type=X].observedGeneration
	syncedObsGenPath := fieldpath.MakePathOrDie("status", "conditions",
		fieldpath.KeyByFields("type", "Synced"), "observedGeneration")
	readyObsGenPath := fieldpath.MakePathOrDie("status", "conditions",
		fieldpath.KeyByFields("type", "Ready"), "observedGeneration")

	for _, entry := range managedFields {
		// Skip non-status updates
		if entry.Subresource != "status" && entry.Subresource != "" {
			continue
		}

		if entry.FieldsV1 == nil || len(entry.FieldsV1.Raw) == 0 {
			continue
		}

		// Parse the field set
		var set fieldpath.Set
		if err := set.FromJSON(bytes.NewReader(entry.FieldsV1.Raw)); err != nil {
			continue
		}

		// Check if this manager owns status.observedGeneration
		if set.Has(obsGenPath) {
			return entry.Manager
		}

		// Fallback: check condition observedGeneration (for Crossplane)
		if set.Has(syncedObsGenPath) || set.Has(readyObsGenPath) {
			return entry.Manager
		}
	}

	return ""
}
