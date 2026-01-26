package v1alpha1

// Annotation keys for kausality.io annotations on Kubernetes resources.
const (
	// TraceAnnotation stores the causal trace as JSON.
	// Value: JSON array of Hop objects.
	TraceAnnotation = "kausality.io/trace"

	// TraceMetadataPrefix is the prefix for custom trace metadata annotations.
	// Annotations like "kausality.io/trace-ticket" become Labels["ticket"] in the trace.
	TraceMetadataPrefix = "kausality.io/trace-"

	// ControllersAnnotation stores hashes of users who update parent status.
	// Value: comma-separated 5-char base36 hashes (max 5).
	ControllersAnnotation = "kausality.io/controllers"

	// UpdatersAnnotation stores hashes of users who update child spec.
	// Value: comma-separated 5-char base36 hashes (max 5).
	UpdatersAnnotation = "kausality.io/updaters"

	// PhaseAnnotation stores the lifecycle phase of a parent resource.
	// Value: "initializing" or "initialized".
	PhaseAnnotation = "kausality.io/phase"

	// ApprovalsAnnotation stores approved child mutations.
	// Value: JSON array of Approval objects.
	ApprovalsAnnotation = "kausality.io/approvals"

	// RejectionsAnnotation stores rejected child mutations.
	// Value: JSON array of Rejection objects.
	RejectionsAnnotation = "kausality.io/rejections"

	// FreezeAnnotation indicates a parent is frozen (all child mutations blocked).
	// Value: JSON Freeze object, or legacy "true".
	FreezeAnnotation = "kausality.io/freeze"

	// SnoozeAnnotation indicates drift callbacks are temporarily suppressed.
	// Value: JSON Snooze object, or legacy RFC3339 timestamp.
	SnoozeAnnotation = "kausality.io/snooze"
)

// Phase values for the PhaseAnnotation.
const (
	PhaseValueInitializing = "initializing"
	PhaseValueInitialized  = "initialized"
)

// MaxHashes is the maximum number of user hashes stored in annotations.
const MaxHashes = 5

// Approval modes for the Approval.Mode field.
const (
	// ApprovalModeOnce removes the approval after first use.
	ApprovalModeOnce = "once"

	// ApprovalModeGeneration is valid while parent.generation == approval.generation.
	ApprovalModeGeneration = "generation"

	// ApprovalModeAlways is permanent and never auto-pruned.
	ApprovalModeAlways = "always"
)
