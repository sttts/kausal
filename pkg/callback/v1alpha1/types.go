// Package v1alpha1 contains API types for drift notification callbacks.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// GroupName is the API group name.
	GroupName = "kausality.io"
	// Version is the API version.
	Version = "v1alpha1"
)

// DriftReportPhase indicates the phase of a drift report.
type DriftReportPhase string

const (
	// DriftReportPhaseDetected indicates drift was detected.
	DriftReportPhaseDetected DriftReportPhase = "Detected"
	// DriftReportPhaseResolved indicates drift was resolved.
	DriftReportPhaseResolved DriftReportPhase = "Resolved"
)

// DriftReport is sent to webhook endpoints when drift is detected.
// This is a transient type with no persistence, so it only has TypeMeta.
type DriftReport struct {
	metav1.TypeMeta `json:",inline"`

	// spec contains the drift report details.
	// +required
	Spec DriftReportSpec `json:"spec"`
}

// DriftReportSpec contains the details of a drift report.
type DriftReportSpec struct {
	// id uniquely identifies this drift occurrence.
	// Format: sha256(parent-ref + child-ref + spec-diff-hash)[:16]
	// +required
	ID string `json:"id"`

	// phase indicates whether this is detection or resolution.
	// +required
	Phase DriftReportPhase `json:"phase"`

	// parent is the parent object reference.
	// +required
	Parent ObjectReference `json:"parent"`

	// child is the child object that drifted.
	// +required
	Child ObjectReference `json:"child"`

	// oldObject is the previous state. Only set for UPDATE operations.
	// +optional
	OldObject *runtime.RawExtension `json:"oldObject,omitempty"`

	// newObject is the current/new state of the object.
	// +required
	NewObject runtime.RawExtension `json:"newObject"`

	// request contains admission request context.
	// +required
	Request RequestContext `json:"request"`
}

// ObjectReference identifies a Kubernetes object.
type ObjectReference struct {
	// apiVersion is the API version of the object (e.g., "v1", "apps/v1").
	// +required
	APIVersion string `json:"apiVersion"`

	// kind is the kind of the object (e.g., "ConfigMap", "Deployment").
	// +required
	Kind string `json:"kind"`

	// namespace is the namespace of the object. Empty for cluster-scoped objects.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// name is the name of the object.
	// +required
	Name string `json:"name"`

	// uid is the unique identifier of the object.
	// +optional
	UID types.UID `json:"uid,omitempty"`

	// generation is the generation of the object (metadata.generation).
	// +optional
	Generation int64 `json:"generation,omitempty"`

	// observedGeneration is the observedGeneration from the object's status.
	// Only set for parent objects. Compare with generation to determine if stable.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// controllerManager is the manager that owns status.observedGeneration.
	// Only set for parent objects. Identifies the controller.
	// +optional
	ControllerManager string `json:"controllerManager,omitempty"`

	// lifecyclePhase is the lifecycle phase (Initializing, Ready, Deleting).
	// Only set for parent objects.
	// +optional
	LifecyclePhase string `json:"lifecyclePhase,omitempty"`
}

// RequestContext contains information about the admission request.
type RequestContext struct {
	// user is the username of the requestor.
	// +required
	User string `json:"user"`

	// groups are the groups the user belongs to.
	// +optional
	Groups []string `json:"groups,omitempty"`

	// uid is the unique identifier of the request.
	// +required
	UID string `json:"uid"`

	// fieldManager is the field manager for the request.
	// +optional
	FieldManager string `json:"fieldManager,omitempty"`

	// operation is the type of operation (CREATE, UPDATE, DELETE).
	// +required
	Operation string `json:"operation"`

	// dryRun indicates this is a dry-run request where changes won't be persisted.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

// DriftReportResponse is the response from a drift report webhook.
type DriftReportResponse struct {
	metav1.TypeMeta `json:",inline"`

	// acknowledged indicates the webhook received the report.
	// +required
	Acknowledged bool `json:"acknowledged"`

	// error is set if the webhook had a problem processing the report.
	// +optional
	Error string `json:"error,omitempty"`
}
