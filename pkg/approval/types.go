// Package approval re-exports types from api/v1alpha1 for backward compatibility.
package approval

import (
	"github.com/kausality-io/kausality/api/v1alpha1"
)

// Annotation keys - re-exported from api/v1alpha1.
const (
	ApprovalsAnnotation  = v1alpha1.ApprovalsAnnotation
	RejectionsAnnotation = v1alpha1.RejectionsAnnotation
	FreezeAnnotation     = v1alpha1.FreezeAnnotation
	SnoozeAnnotation     = v1alpha1.SnoozeAnnotation
)

// Approval modes - re-exported from api/v1alpha1.
const (
	ModeOnce       = v1alpha1.ApprovalModeOnce
	ModeGeneration = v1alpha1.ApprovalModeGeneration
	ModeAlways     = v1alpha1.ApprovalModeAlways
)

// Types - re-exported from api/v1alpha1.
type (
	Approval  = v1alpha1.Approval
	Rejection = v1alpha1.Rejection
	ChildRef  = v1alpha1.ChildRef
	Freeze    = v1alpha1.Freeze
	Snooze    = v1alpha1.Snooze
)

// Functions - re-exported from api/v1alpha1.
var (
	ParseApprovals   = v1alpha1.ParseApprovals
	ParseRejections  = v1alpha1.ParseRejections
	MarshalApprovals = v1alpha1.MarshalApprovals
	ParseFreeze      = v1alpha1.ParseFreeze
	MarshalFreeze    = v1alpha1.MarshalFreeze
	ParseSnooze      = v1alpha1.ParseSnooze
	MarshalSnooze    = v1alpha1.MarshalSnooze
)
