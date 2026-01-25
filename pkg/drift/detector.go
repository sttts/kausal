package drift

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kausality-io/kausality/pkg/controller"
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
// Deprecated: Use DetectWithUsername for proper controller identification via user hash tracking.
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

	case PhaseInitialized:
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

// DetectWithUsername checks whether a mutation would be considered drift.
// It uses user hash tracking to identify if the request comes from the controller.
// childUpdaters contains the current updater hashes from the child's annotation (before this update).
func (d *Detector) DetectWithUsername(ctx context.Context, obj client.Object, username string, childUpdaters []string) (*DriftResult, error) {
	// Resolve parent
	parentState, err := d.resolver.ResolveParent(ctx, obj)
	if err != nil {
		return &DriftResult{
			Allowed: false,
			Reason:  fmt.Sprintf("failed to resolve parent: %v", err),
		}, nil
	}

	// No controller owner reference - allow (not managed by a controller, can never be drift)
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

	case PhaseInitialized:
		// Fall through to drift detection
	}

	// Check if request comes from the controller using user hash tracking
	isController, canDetermine := d.isControllerByHash(parentState, username, childUpdaters)

	if !canDetermine {
		// Can't determine controller identity - be lenient
		result.Allowed = true
		result.DriftDetected = false
		result.Reason = "cannot determine controller identity (multiple updaters, no parent controllers annotation)"
		return result, nil
	}

	if !isController {
		// Different actor - not drift, but a new causal origin
		result.Allowed = true
		result.DriftDetected = false
		userHash := controller.HashUsername(username)
		result.Reason = fmt.Sprintf("change by different actor (hash %s)", userHash)
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

// isControllerByHash checks if the request comes from the controller using user hash tracking.
// Returns (isController, canDetermine).
func (d *Detector) isControllerByHash(parentState *ParentState, username string, childUpdaters []string) (bool, bool) {
	userHash := controller.HashUsername(username)

	// Case 1: Single updater on child - that's the controller
	if len(childUpdaters) == 1 {
		return userHash == childUpdaters[0], true
	}

	// Case 2: Multiple updaters + parent has controllers - use intersection
	if len(childUpdaters) > 1 && len(parentState.Controllers) > 0 {
		intersection := intersectHashes(childUpdaters, parentState.Controllers)
		if len(intersection) > 0 {
			return containsHash(intersection, userHash), true
		}
	}

	// Case 3: No updaters yet (CREATE) - current user is the first/only updater
	if len(childUpdaters) == 0 {
		// This is a CREATE - the current user will be the only updater
		return true, true
	}

	// Case 4: Can't determine (multiple updaters, no parent controllers)
	return false, false
}

// intersectHashes returns hashes present in both lists.
func intersectHashes(a, b []string) []string {
	set := make(map[string]struct{})
	for _, h := range a {
		set[h] = struct{}{}
	}
	var result []string
	for _, h := range b {
		if _, ok := set[h]; ok {
			result = append(result, h)
		}
	}
	return result
}

// containsHash checks if a hash is in the list.
func containsHash(hashes []string, hash string) bool {
	for _, h := range hashes {
		if h == hash {
			return true
		}
	}
	return false
}

// parseUpdaterHashes extracts updater hashes from the child object's annotation.
func ParseUpdaterHashes(obj client.Object) []string {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return nil
	}
	updaters := annotations[controller.UpdatersAnnotation]
	if updaters == "" {
		return nil
	}
	parts := strings.Split(updaters, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
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

	case PhaseInitialized:
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
