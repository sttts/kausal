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
		return hasCondition(state.Conditions, ConditionTypeInitialized, metav1.ConditionTrue)
	case DetectByReadyCondition:
		return hasCondition(state.Conditions, ConditionTypeReady, metav1.ConditionTrue)
	case DetectByObservedGeneration:
		// For observedGeneration to indicate "initialized", we need:
		// 1. HasObservedGeneration (gen == obsGen from condition)
		// 2. A "ready-like" condition is True (Ready, Available, or Initialized)
		//
		// We specifically do NOT use Synced=True alone because:
		// - Crossplane sets Synced=True when it sends requests to create children
		// - But Ready=False until children are actually ready
		// - During child creation, the controller needs to update children
		// - Those updates should be allowed until fully ready
		if !state.HasObservedGeneration {
			return false
		}
		return hasCondition(state.Conditions, ConditionTypeReady, metav1.ConditionTrue) ||
			hasCondition(state.Conditions, ConditionTypeAvailable, metav1.ConditionTrue) ||
			hasCondition(state.Conditions, ConditionTypeInitialized, metav1.ConditionTrue)
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
