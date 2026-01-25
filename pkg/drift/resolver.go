package drift

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	// Check for deletion timestamp
	if parent.GetDeletionTimestamp() != nil {
		state.DeletionTimestamp = parent.GetDeletionTimestamp()
	}

	// Check annotations
	if annotations := parent.GetAnnotations(); annotations != nil {
		// Read phase annotation
		state.PhaseFromAnnotation = annotations[PhaseAnnotation]
		if state.PhaseFromAnnotation == PhaseValueInitialized {
			state.IsInitialized = true
		}

		// Extract controller hashes from kausality.io/controllers annotation
		if controllers := annotations[controller.ControllersAnnotation]; controllers != "" {
			state.Controllers = controller.ParseHashes(controllers)
		}
	}

	return state
}

// extractConditionObservedGeneration extracts observedGeneration from Synced or Ready conditions.
// Returns the observedGeneration and whether it was found.
// Prefers Synced condition, falls back to Ready.
func extractConditionObservedGeneration(status map[string]interface{}) (int64, bool) {
	conditionsRaw, ok, _ := unstructured.NestedSlice(status, "conditions")
	if !ok {
		return 0, false
	}

	// Single pass: find Synced or Ready, preferring Synced
	var readyObsGen int64
	var foundReady bool

	for _, c := range conditionsRaw {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(condMap, "type")
		obsGen, hasObsGen, _ := unstructured.NestedInt64(condMap, "observedGeneration")
		if !hasObsGen {
			continue
		}
		if condType == ConditionTypeSynced {
			return obsGen, true // Synced takes priority, return immediately
		}
		if condType == ConditionTypeReady {
			readyObsGen = obsGen
			foundReady = true
		}
	}

	return readyObsGen, foundReady
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
