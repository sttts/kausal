// Package drift provides drift detection for Kubernetes controllers.
package drift

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DriftResult represents the outcome of drift detection.
type DriftResult struct {
	// Allowed indicates whether the mutation should be allowed.
	Allowed bool
	// Reason provides a human-readable explanation for the decision.
	Reason string
	// DriftDetected indicates whether drift was detected (parent gen == obsGen).
	DriftDetected bool
	// ParentRef identifies the parent object, if found.
	ParentRef *ParentRef
	// LifecyclePhase indicates the parent's lifecycle phase.
	LifecyclePhase LifecyclePhase
}

// ParentRef identifies the parent object.
type ParentRef struct {
	// APIVersion of the parent object.
	APIVersion string
	// Kind of the parent object.
	Kind string
	// Namespace of the parent object (empty for cluster-scoped).
	Namespace string
	// Name of the parent object.
	Name string
}

// String returns a human-readable representation of the parent reference.
func (p *ParentRef) String() string {
	if p.Namespace != "" {
		return p.APIVersion + "/" + p.Kind + ":" + p.Namespace + "/" + p.Name
	}
	return p.APIVersion + "/" + p.Kind + ":" + p.Name
}

// ParentState holds parent object state for drift detection.
type ParentState struct {
	// Ref identifies the parent object.
	Ref ParentRef
	// Generation is the parent's metadata.generation.
	Generation int64
	// ObservedGeneration is the parent's status.observedGeneration.
	ObservedGeneration int64
	// HasObservedGeneration indicates whether status.observedGeneration exists.
	HasObservedGeneration bool
	// DeletionTimestamp is set if the parent is being deleted.
	DeletionTimestamp *metav1.Time
	// Conditions are the parent's status conditions for lifecycle detection.
	Conditions []metav1.Condition
	// IsInitialized indicates whether the parent has completed initialization.
	IsInitialized bool
}

// LifecyclePhase represents the lifecycle phase of a parent object.
type LifecyclePhase string

const (
	// PhaseInitializing indicates the parent is still initializing.
	PhaseInitializing LifecyclePhase = "Initializing"
	// PhaseReady indicates the parent is in steady state.
	PhaseReady LifecyclePhase = "Ready"
	// PhaseDeleting indicates the parent is being deleted.
	PhaseDeleting LifecyclePhase = "Deleting"
)

// InitializationDetector determines how to detect initialization.
type InitializationDetector int

const (
	// DetectByInitializedCondition checks for Initialized=True condition.
	DetectByInitializedCondition InitializationDetector = iota
	// DetectByReadyCondition checks for Ready=True condition.
	DetectByReadyCondition
	// DetectByObservedGeneration checks for status.observedGeneration existence.
	DetectByObservedGeneration
)

// DefaultDetectionOrder is the default priority order for initialization detection.
var DefaultDetectionOrder = []InitializationDetector{
	DetectByInitializedCondition,
	DetectByReadyCondition,
	DetectByObservedGeneration,
}
