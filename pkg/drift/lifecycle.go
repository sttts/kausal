package drift

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LifecycleDetector determines the lifecycle phase of a parent object.
type LifecycleDetector struct {
	// DetectionOrder specifies the priority order for initialization detection.
	// Defaults to DefaultDetectionOrder if nil.
	DetectionOrder []InitializationDetector
}

// NewLifecycleDetector creates a new LifecycleDetector with default settings.
func NewLifecycleDetector() *LifecycleDetector {
	return &LifecycleDetector{
		DetectionOrder: DefaultDetectionOrder,
	}
}

// DetectPhase determines the lifecycle phase of a parent object.
func (d *LifecycleDetector) DetectPhase(state *ParentState) LifecyclePhase {
	if state == nil {
		return PhaseInitialized
	}

	// Check deletion first - takes precedence
	if state.DeletionTimestamp != nil {
		return PhaseDeleting
	}

	// Check if already marked as initialized via annotation
	if state.IsInitialized {
		return PhaseInitialized
	}

	// Check initialization using configured detection order
	detectionOrder := d.DetectionOrder
	if len(detectionOrder) == 0 {
		detectionOrder = DefaultDetectionOrder
	}

	for _, detector := range detectionOrder {
		if d.checkInitialized(state, detector) {
			return PhaseInitialized
		}
	}

	return PhaseInitializing
}

// checkInitialized checks if the parent is initialized using the specified detector.
func (d *LifecycleDetector) checkInitialized(state *ParentState, detector InitializationDetector) bool {
	switch detector {
	case DetectByInitializedCondition:
		return hasCondition(state.Conditions, "Initialized", metav1.ConditionTrue)
	case DetectByReadyCondition:
		return hasCondition(state.Conditions, "Ready", metav1.ConditionTrue)
	case DetectByObservedGeneration:
		return state.HasObservedGeneration
	default:
		return false
	}
}

// hasCondition checks if the conditions slice contains a condition with the given type and status.
func hasCondition(conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus) bool {
	for _, c := range conditions {
		if c.Type == conditionType && c.Status == status {
			return true
		}
	}
	return false
}

// FindCondition returns the condition with the given type, or nil if not found.
func FindCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// IsDeleting returns true if the parent is in the deletion phase.
func IsDeleting(state *ParentState) bool {
	return state != nil && state.DeletionTimestamp != nil
}

// IsInitializing returns true if the parent is in the initialization phase.
func IsInitializing(state *ParentState, detector *LifecycleDetector) bool {
	if detector == nil {
		detector = NewLifecycleDetector()
	}
	return detector.DetectPhase(state) == PhaseInitializing
}
