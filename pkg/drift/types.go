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
	// ParentState contains the parent's generation and controller info.
	ParentState *ParentState
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
	// Controllers contains user hashes from kausality.io/controllers annotation.
	// These are users who have updated the parent's status.
	Controllers []string
	// DeletionTimestamp is set if the parent is being deleted.
	DeletionTimestamp *metav1.Time
	// Conditions are the parent's status conditions for lifecycle detection.
	Conditions []metav1.Condition
	// IsInitialized indicates whether the parent has completed initialization.
	IsInitialized bool
	// PhaseFromAnnotation is the value of kausality.io/phase annotation.
	// Used to determine if phase needs to be recorded (lazy fetch optimization).
	PhaseFromAnnotation string
}

// LifecyclePhase represents the lifecycle phase of a parent object.
type LifecyclePhase string

const (
	// PhaseInitializing indicates the parent is still initializing.
	PhaseInitializing LifecyclePhase = "Initializing"
	// PhaseInitialized indicates the parent is in steady state.
	PhaseInitialized LifecyclePhase = "Initialized"
	// PhaseDeleting indicates the parent is being deleted.
	PhaseDeleting LifecyclePhase = "Deleting"
)

// PhaseAnnotation stores the lifecycle phase of a parent resource.
const PhaseAnnotation = "kausality.io/phase"

// Phase values for the PhaseAnnotation.
const (
	PhaseValueInitializing = "initializing"
	PhaseValueInitialized  = "initialized"
	// PhaseDeleting is not stored - derived from deletionTimestamp
)

// Condition types used for initialization and observedGeneration detection.
const (
	ConditionTypeInitialized = "Initialized"
	ConditionTypeReady       = "Ready"
	ConditionTypeAvailable   = "Available"
	ConditionTypeSynced      = "Synced"
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
