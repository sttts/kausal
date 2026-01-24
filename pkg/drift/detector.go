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
func (d *Detector) Detect(ctx context.Context, obj client.Object) (*DriftResult, error) {
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

	// Core drift detection: compare generation vs observedGeneration
	if parentState.Generation != parentState.ObservedGeneration {
		// Parent spec changed - expected change
		result.Allowed = true
		result.DriftDetected = false
		result.Reason = fmt.Sprintf("expected change: parent generation (%d) != observedGeneration (%d)",
			parentState.Generation, parentState.ObservedGeneration)
		return result, nil
	}

	// Drift detected: generation == observedGeneration but child is being mutated
	result.Allowed = true // Phase 1: logging only, always allow
	result.DriftDetected = true
	result.Reason = fmt.Sprintf("drift detected: parent generation (%d) == observedGeneration (%d)",
		parentState.Generation, parentState.ObservedGeneration)

	return result, nil
}

// DetectFromState checks for drift given an already-resolved parent state.
// This is useful when the parent state is already available.
func (d *Detector) DetectFromState(parentState *ParentState) *DriftResult {
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
