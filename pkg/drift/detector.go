package drift

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Detector detects drift by comparing parent generation with observedGeneration.
type Detector struct {
	resolver          *ParentResolver
	lifecycleDetector *LifecycleDetector
}

// NewDetector creates a new Detector.
func NewDetector(c client.Client) *Detector {
	return &Detector{
		resolver:          NewParentResolver(c),
		lifecycleDetector: NewLifecycleDetector(),
	}
}

// DetectorOption configures a Detector.
type DetectorOption func(*Detector)

// WithLifecycleDetector configures a custom lifecycle detector.
func WithLifecycleDetector(ld *LifecycleDetector) DetectorOption {
	return func(d *Detector) {
		d.lifecycleDetector = ld
	}
}

// NewDetectorWithOptions creates a new Detector with options.
func NewDetectorWithOptions(c client.Client, opts ...DetectorOption) *Detector {
	d := NewDetector(c)
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Detect checks whether a mutation to the given object would be considered drift.
// It resolves the controller parent and compares generation with observedGeneration.
// Deprecated: Use DetectWithFieldManager for proper controller identification.
func (d *Detector) Detect(ctx context.Context, obj client.Object) (*DriftResult, error) {
	return d.DetectWithFieldManager(ctx, obj, "")
}

// DetectWithFieldManager checks whether a mutation would be considered drift.
// It uses the fieldManager to identify if the request comes from the controller.
func (d *Detector) DetectWithFieldManager(ctx context.Context, obj client.Object, fieldManager string) (*DriftResult, error) {
	// Resolve parent
	parentState, err := d.resolver.ResolveParent(ctx, obj)
	if err != nil {
		return &DriftResult{
			Allowed: false,
			Reason:  fmt.Sprintf("failed to resolve parent: %v", err),
		}, nil
	}

	// No controller owner reference - allow (not managed by a controller)
	if parentState == nil {
		return &DriftResult{
			Allowed: true,
			Reason:  "no controller owner reference",
		}, nil
	}

	// Determine lifecycle phase
	phase := d.lifecycleDetector.DetectPhase(parentState)

	result := &DriftResult{
		ParentRef:      &parentState.Ref,
		ParentState:    parentState,
		LifecyclePhase: phase,
	}

	// Handle lifecycle phases
	switch phase {
	case PhaseDeleting:
		result.Allowed = true
		result.Reason = "parent is being deleted (cleanup phase)"
		return result, nil

	case PhaseInitializing:
		result.Allowed = true
		result.Reason = "parent is initializing"
		return result, nil

	case PhaseReady:
		// Fall through to drift detection
	}

	// Check if request comes from the controller
	isController := d.isControllerRequest(parentState, fieldManager)

	if !isController {
		// Different actor - not drift, but a new causal origin
		// (trace propagation will create a new trace for this)
		result.Allowed = true
		result.DriftDetected = false
		result.Reason = fmt.Sprintf("change by different actor %q (controller is %q)",
			fieldManager, parentState.ControllerManager)
		return result, nil
	}

	// Request is from the controller - check generation vs observedGeneration
	if parentState.Generation != parentState.ObservedGeneration {
		// Parent spec changed - expected change
		result.Allowed = true
		result.DriftDetected = false
		result.Reason = fmt.Sprintf("expected change: parent generation (%d) != observedGeneration (%d)",
			parentState.Generation, parentState.ObservedGeneration)
		return result, nil
	}

	// Controller is updating but parent hasn't changed - drift
	result.Allowed = true // Phase 1: logging only, always allow
	result.DriftDetected = true
	result.Reason = fmt.Sprintf("drift detected: parent generation (%d) == observedGeneration (%d)",
		parentState.Generation, parentState.ObservedGeneration)

	return result, nil
}

// isControllerRequest checks if the request comes from the controller.
func (d *Detector) isControllerRequest(parentState *ParentState, fieldManager string) bool {
	// If we don't know the controller manager, we can't determine who the controller is.
	// This handles cases where parent doesn't have managedFields (older objects).
	// In this case, we cannot reliably detect drift - treat as controller to allow changes.
	if parentState.ControllerManager == "" {
		return true
	}

	// If the request has a fieldManager that matches the controller, it's definitely the controller.
	if fieldManager == parentState.ControllerManager {
		return true
	}

	// If the request has a non-empty fieldManager that doesn't match, it's a different actor.
	// This allows us to distinguish intentional changes by users, HPA, etc.
	if fieldManager != "" {
		return false
	}

	// Empty fieldManager is ambiguous - controllers often don't set it.
	// Treat as potentially controller and check for drift.
	return true
}

// DetectFromState checks for drift given an already-resolved parent state.
// This is useful when the parent state is already available.
// Deprecated: Use DetectFromStateWithFieldManager for proper controller identification.
func (d *Detector) DetectFromState(parentState *ParentState) *DriftResult {
	return d.DetectFromStateWithFieldManager(parentState, "")
}

// DetectFromStateWithFieldManager checks for drift given parent state and fieldManager.
func (d *Detector) DetectFromStateWithFieldManager(parentState *ParentState, fieldManager string) *DriftResult {
	// No parent state - allow
	if parentState == nil {
		return &DriftResult{
			Allowed: true,
			Reason:  "no parent state provided",
		}
	}

	// Determine lifecycle phase
	phase := d.lifecycleDetector.DetectPhase(parentState)

	result := &DriftResult{
		ParentRef:      &parentState.Ref,
		ParentState:    parentState,
		LifecyclePhase: phase,
	}

	// Handle lifecycle phases
	switch phase {
	case PhaseDeleting:
		result.Allowed = true
		result.Reason = "parent is being deleted (cleanup phase)"
		return result

	case PhaseInitializing:
		result.Allowed = true
		result.Reason = "parent is initializing"
		return result

	case PhaseReady:
		// Fall through to drift detection
	}

	// Check if request comes from the controller
	isController := d.isControllerRequest(parentState, fieldManager)

	if !isController {
		// Different actor - not drift, but a new causal origin
		result.Allowed = true
		result.DriftDetected = false
		result.Reason = fmt.Sprintf("change by different actor %q (controller is %q)",
			fieldManager, parentState.ControllerManager)
		return result
	}

	// Core drift detection
	if parentState.Generation != parentState.ObservedGeneration {
		result.Allowed = true
		result.DriftDetected = false
		result.Reason = fmt.Sprintf("expected change: parent generation (%d) != observedGeneration (%d)",
			parentState.Generation, parentState.ObservedGeneration)
		return result
	}

	result.Allowed = true // Phase 1: logging only
	result.DriftDetected = true
	result.Reason = fmt.Sprintf("drift detected: parent generation (%d) == observedGeneration (%d)",
		parentState.Generation, parentState.ObservedGeneration)

	return result
}
