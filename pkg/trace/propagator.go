package trace

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kausality-io/kausality/pkg/drift"
)

// Propagator handles trace creation and propagation.
type Propagator struct {
	client   client.Client
	resolver *drift.ParentResolver
}

// NewPropagator creates a new Propagator.
func NewPropagator(c client.Client) *Propagator {
	return &Propagator{
		client:   c,
		resolver: drift.NewParentResolver(c),
	}
}

// PropagationResult contains the result of trace propagation.
type PropagationResult struct {
	// Trace is the trace to set on the object.
	Trace Trace
	// IsOrigin indicates this is a new trace (no parent trace to extend).
	IsOrigin bool
	// ParentTrace is the parent's trace (nil if origin).
	ParentTrace Trace
}

// Propagate determines the trace for a mutated object.
// Deprecated: Use PropagateWithFieldManager for proper controller identification.
func (p *Propagator) Propagate(ctx context.Context, obj client.Object, user string) (*PropagationResult, error) {
	return p.PropagateWithFieldManager(ctx, obj, user, "")
}

// PropagateWithFieldManager determines the trace for a mutated object.
// For origins (no parent, parent not reconciling, or different actor), creates a new trace.
// For controller hops (same fieldManager as controller, parent reconciling), extends parent's trace.
func (p *Propagator) PropagateWithFieldManager(ctx context.Context, obj client.Object, user string, fieldManager string) (*PropagationResult, error) {
	// Resolve parent state
	parentState, err := p.resolver.ResolveParent(ctx, obj)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve parent: %w", err)
	}

	// Determine if this is an origin or a hop
	isOrigin := p.isOrigin(parentState, fieldManager)

	// Get GVK info
	gvk := obj.GetObjectKind().GroupVersionKind()
	apiVersion := gvk.GroupVersion().String()
	if apiVersion == "/" {
		// Fallback for core types
		apiVersion = "v1"
	}

	result := &PropagationResult{
		IsOrigin: isOrigin,
	}

	// Extract trace labels from this object's annotations
	labels := ExtractTraceLabels(obj.GetAnnotations())

	if isOrigin {
		// Create new trace starting with this object
		result.Trace = Trace{
			NewHopWithLabels(apiVersion, gvk.Kind, obj.GetName(), obj.GetGeneration(), user, labels),
		}
	} else {
		// Get parent's trace
		parentTrace, err := p.getParentTrace(ctx, parentState)
		if err != nil {
			return nil, fmt.Errorf("failed to get parent trace: %w", err)
		}
		result.ParentTrace = parentTrace

		// Extend trace with new hop (each hop has its own labels, no inheritance)
		hop := NewHopWithLabels(apiVersion, gvk.Kind, obj.GetName(), obj.GetGeneration(), user, labels)
		result.Trace = parentTrace.Append(hop)
	}

	return result, nil
}

// isOrigin determines if this mutation starts a new trace.
// Origin conditions:
// - No controller ownerReference
// - Parent has generation == observedGeneration (not reconciling)
// - Request fieldManager doesn't match parent's controller manager (different actor)
func (p *Propagator) isOrigin(parentState *drift.ParentState, fieldManager string) bool {
	// No parent = origin
	if parentState == nil {
		return true
	}

	// Parent not reconciling (gen == obsGen) = origin (drift case)
	if parentState.Generation == parentState.ObservedGeneration {
		return true
	}

	// Check if request is from the controller
	if !p.isControllerRequest(parentState, fieldManager) {
		// Different actor = origin (even if parent is reconciling)
		return true
	}

	// Controller is reconciling = hop (extend parent trace)
	return false
}

// isControllerRequest checks if the request comes from the controller.
func (p *Propagator) isControllerRequest(parentState *drift.ParentState, fieldManager string) bool {
	// If we don't know the controller manager, fall back to assuming controller
	if parentState.ControllerManager == "" {
		return true
	}

	// If fieldManager is empty, fall back to assuming controller
	if fieldManager == "" {
		return true
	}

	// Compare field managers
	return fieldManager == parentState.ControllerManager
}

// getParentTrace retrieves the trace from the parent object.
func (p *Propagator) getParentTrace(ctx context.Context, parentState *drift.ParentState) (Trace, error) {
	if parentState == nil {
		return nil, nil
	}

	// Fetch the parent object
	gv, err := schema.ParseGroupVersion(parentState.Ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid parent API version: %w", err)
	}

	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(gv.WithKind(parentState.Ref.Kind))

	key := client.ObjectKey{
		Namespace: parentState.Ref.Namespace,
		Name:      parentState.Ref.Name,
	}

	if err := p.client.Get(ctx, key, parent); err != nil {
		return nil, fmt.Errorf("failed to get parent: %w", err)
	}

	return GetTraceFromObject(parent)
}

// GetTraceFromObject extracts the trace from an object's annotations.
func GetTraceFromObject(obj client.Object) (Trace, error) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return nil, nil
	}

	traceStr, ok := annotations[TraceAnnotation]
	if !ok || traceStr == "" {
		return nil, nil
	}

	return Parse(traceStr)
}

// SetTraceOnObject sets the trace annotation on an object.
func SetTraceOnObject(obj *unstructured.Unstructured, trace Trace) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[TraceAnnotation] = trace.String()
	obj.SetAnnotations(annotations)
}
