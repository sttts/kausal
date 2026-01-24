// Package approval provides types and logic for drift approval/rejection.
package approval

import (
	"encoding/json"
	"fmt"
)

// Annotation keys for approvals and rejections on parent resources.
const (
	ApprovalsAnnotation  = "kausality.io/approvals"
	RejectionsAnnotation = "kausality.io/rejections"
)

// Approval modes.
const (
	// ModeOnce removes the approval after first use.
	ModeOnce = "once"
	// ModeGeneration is valid while parent.generation == approval.generation.
	ModeGeneration = "generation"
	// ModeAlways is permanent and never auto-pruned.
	ModeAlways = "always"
)

// Approval represents an approval for a child resource mutation.
// Stored in parent's kausality.io/approvals annotation.
type Approval struct {
	// APIVersion of the approved child resource.
	APIVersion string `json:"apiVersion"`
	// Kind of the approved child resource.
	Kind string `json:"kind"`
	// Name of the approved child resource.
	Name string `json:"name"`
	// Generation is the parent generation this approval is valid for.
	// Required for ModeOnce and ModeGeneration, ignored for ModeAlways.
	Generation int64 `json:"generation,omitempty"`
	// Mode determines approval validity and pruning behavior.
	// One of: once, generation, always. Defaults to "once".
	Mode string `json:"mode,omitempty"`
}

// Rejection represents a rejection for a child resource mutation.
// Stored in parent's kausality.io/rejections annotation.
type Rejection struct {
	// APIVersion of the rejected child resource.
	APIVersion string `json:"apiVersion"`
	// Kind of the rejected child resource.
	Kind string `json:"kind"`
	// Name of the rejected child resource.
	Name string `json:"name"`
	// Generation is the parent generation this rejection applies to.
	// If set, only rejects when parent.generation == rejection.generation.
	Generation int64 `json:"generation,omitempty"`
	// Reason explains why the mutation is rejected.
	Reason string `json:"reason"`
}

// ChildRef identifies a child resource being mutated.
type ChildRef struct {
	APIVersion string
	Kind       string
	Name       string
}

// Matches checks if this approval matches the given child.
func (a *Approval) Matches(child ChildRef) bool {
	return a.APIVersion == child.APIVersion &&
		a.Kind == child.Kind &&
		a.Name == child.Name
}

// IsValid checks if this approval is valid for the given parent generation.
func (a *Approval) IsValid(parentGeneration int64) bool {
	mode := a.Mode
	if mode == "" {
		mode = ModeOnce // Default
	}

	switch mode {
	case ModeAlways:
		return true
	case ModeOnce, ModeGeneration:
		return a.Generation == parentGeneration
	default:
		return false
	}
}

// Matches checks if this rejection matches the given child.
func (r *Rejection) Matches(child ChildRef) bool {
	return r.APIVersion == child.APIVersion &&
		r.Kind == child.Kind &&
		r.Name == child.Name
}

// IsActive checks if this rejection is active for the given parent generation.
func (r *Rejection) IsActive(parentGeneration int64) bool {
	// If generation is 0 (not set), rejection is always active
	if r.Generation == 0 {
		return true
	}
	return r.Generation == parentGeneration
}

// ParseApprovals parses the approvals annotation value.
func ParseApprovals(annotationValue string) ([]Approval, error) {
	if annotationValue == "" {
		return nil, nil
	}

	var approvals []Approval
	if err := json.Unmarshal([]byte(annotationValue), &approvals); err != nil {
		return nil, fmt.Errorf("invalid approvals annotation: %w", err)
	}
	return approvals, nil
}

// ParseRejections parses the rejections annotation value.
func ParseRejections(annotationValue string) ([]Rejection, error) {
	if annotationValue == "" {
		return nil, nil
	}

	var rejections []Rejection
	if err := json.Unmarshal([]byte(annotationValue), &rejections); err != nil {
		return nil, fmt.Errorf("invalid rejections annotation: %w", err)
	}
	return rejections, nil
}

// MarshalApprovals marshals approvals to JSON for annotation.
func MarshalApprovals(approvals []Approval) (string, error) {
	if len(approvals) == 0 {
		return "", nil
	}
	data, err := json.Marshal(approvals)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
