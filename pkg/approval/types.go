// Package approval provides types and logic for drift approval/rejection.
package approval

import (
	"encoding/json"
	"fmt"
	"time"
)

// Annotation keys for approvals and rejections on parent resources.
const (
	ApprovalsAnnotation  = "kausality.io/approvals"
	RejectionsAnnotation = "kausality.io/rejections"
	FreezeAnnotation     = "kausality.io/freeze"
	SnoozeAnnotation     = "kausality.io/snooze"
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

// Freeze represents a freeze lockdown on a parent resource.
// When set, ALL child mutations are blocked.
// Stored in parent's kausality.io/freeze annotation as JSON.
type Freeze struct {
	// User who applied the freeze.
	User string `json:"user,omitempty"`
	// Message explaining why the freeze was applied.
	Message string `json:"message,omitempty"`
	// At is when the freeze was applied.
	At time.Time `json:"at,omitempty"`
}

// Snooze represents a snooze period on a parent resource.
// During snooze, drift callbacks are suppressed.
// Stored in parent's kausality.io/snooze annotation as JSON.
type Snooze struct {
	// Expiry is when the snooze expires.
	Expiry time.Time `json:"expiry"`
	// User who applied the snooze.
	User string `json:"user,omitempty"`
	// Message explaining why the snooze was applied.
	Message string `json:"message,omitempty"`
}

// Matches checks if this approval matches the given child.
// Supports wildcards: "*" matches any value for apiVersion, kind, or name.
func (a *Approval) Matches(child ChildRef) bool {
	return matchField(a.APIVersion, child.APIVersion) &&
		matchField(a.Kind, child.Kind) &&
		matchField(a.Name, child.Name)
}

// matchField checks if a pattern matches a value.
// "*" matches any value.
func matchField(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	return pattern == value
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
// Supports wildcards: "*" matches any value for apiVersion, kind, or name.
func (r *Rejection) Matches(child ChildRef) bool {
	return matchField(r.APIVersion, child.APIVersion) &&
		matchField(r.Kind, child.Kind) &&
		matchField(r.Name, child.Name)
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

// ParseFreeze parses the freeze annotation value.
// Returns nil if the annotation is empty or not set.
func ParseFreeze(annotationValue string) (*Freeze, error) {
	if annotationValue == "" {
		return nil, nil
	}

	// Support legacy format: plain "true" value
	if annotationValue == "true" {
		return &Freeze{}, nil
	}

	var freeze Freeze
	if err := json.Unmarshal([]byte(annotationValue), &freeze); err != nil {
		return nil, fmt.Errorf("invalid freeze annotation: %w", err)
	}
	return &freeze, nil
}

// MarshalFreeze marshals a freeze to JSON for annotation.
func MarshalFreeze(freeze *Freeze) (string, error) {
	if freeze == nil {
		return "", nil
	}
	data, err := json.Marshal(freeze)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// String returns a human-readable description of the freeze.
func (f *Freeze) String() string {
	if f == nil {
		return ""
	}
	msg := "frozen"
	if f.Message != "" {
		msg += ": " + f.Message
	}
	if !f.At.IsZero() {
		msg += fmt.Sprintf(" (since %s)", f.At.Format(time.RFC3339))
	}
	return msg
}

// ParseSnooze parses the snooze annotation value.
// Returns nil if the annotation is empty or not set.
func ParseSnooze(annotationValue string) (*Snooze, error) {
	if annotationValue == "" {
		return nil, nil
	}

	// Support legacy format: plain RFC3339 timestamp
	if t, err := time.Parse(time.RFC3339, annotationValue); err == nil {
		return &Snooze{Expiry: t}, nil
	}

	var snooze Snooze
	if err := json.Unmarshal([]byte(annotationValue), &snooze); err != nil {
		return nil, fmt.Errorf("invalid snooze annotation: %w", err)
	}
	return &snooze, nil
}

// MarshalSnooze marshals a snooze to JSON for annotation.
func MarshalSnooze(snooze *Snooze) (string, error) {
	if snooze == nil {
		return "", nil
	}
	data, err := json.Marshal(snooze)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// IsActive checks if the snooze is still active (not expired).
func (s *Snooze) IsActive() bool {
	if s == nil {
		return false
	}
	return time.Now().Before(s.Expiry)
}

// String returns a human-readable description of the snooze.
func (s *Snooze) String() string {
	if s == nil {
		return ""
	}
	msg := fmt.Sprintf("snoozed until %s", s.Expiry.Format(time.RFC3339))
	if s.User != "" {
		msg += " by " + s.User
	}
	if s.Message != "" {
		msg += ": " + s.Message
	}
	return msg
}
