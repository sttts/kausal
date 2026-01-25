package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// SnoozeAnnotation is the annotation key for snooze-until timestamp.
	SnoozeAnnotation = "kausality.io/snooze-until"

	// FreezeAnnotation is the annotation key for freeze lockdown.
	// When set to "true", ALL child mutations are blocked, even expected changes.
	FreezeAnnotation = "kausality.io/freeze"
)

// ObjectRef identifies a Kubernetes object for actions.
type ObjectRef struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// ActionApplier applies drift actions to Kubernetes objects.
type ActionApplier struct {
	client client.Client
}

// NewActionApplier creates a new ActionApplier.
func NewActionApplier(c client.Client) *ActionApplier {
	return &ActionApplier{client: c}
}

// ApplyApproval adds an approval annotation to the parent object.
// The mode can be "once", "generation", or "always".
func (a *ActionApplier) ApplyApproval(ctx context.Context, parent ObjectRef, child ChildRef, mode string) error {
	if mode == "" {
		mode = ModeOnce
	}

	// Fetch the parent object
	parentObj, err := a.fetchObject(ctx, parent)
	if err != nil {
		return fmt.Errorf("failed to fetch parent: %w", err)
	}

	// Get current approvals
	annotations := parentObj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	var approvals []Approval
	if existing := annotations[ApprovalsAnnotation]; existing != "" {
		approvals, err = ParseApprovals(existing)
		if err != nil {
			return fmt.Errorf("failed to parse existing approvals: %w", err)
		}
	}

	// Check if approval already exists
	for i, app := range approvals {
		if app.Matches(child) {
			// Update existing approval
			approvals[i].Mode = mode
			if mode != ModeAlways {
				approvals[i].Generation = parentObj.GetGeneration()
			}
			return a.updateApprovals(ctx, parentObj, annotations, approvals)
		}
	}

	// Add new approval
	approval := Approval{
		APIVersion: child.APIVersion,
		Kind:       child.Kind,
		Name:       child.Name,
		Mode:       mode,
	}
	if mode != ModeAlways {
		approval.Generation = parentObj.GetGeneration()
	}
	approvals = append(approvals, approval)

	return a.updateApprovals(ctx, parentObj, annotations, approvals)
}

// ApplyRejection adds a rejection annotation to the parent object.
func (a *ActionApplier) ApplyRejection(ctx context.Context, parent ObjectRef, child ChildRef, reason string) error {
	if reason == "" {
		reason = "rejected via webhook"
	}

	// Fetch the parent object
	parentObj, err := a.fetchObject(ctx, parent)
	if err != nil {
		return fmt.Errorf("failed to fetch parent: %w", err)
	}

	// Get current rejections
	annotations := parentObj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	var rejections []Rejection
	if existing := annotations[RejectionsAnnotation]; existing != "" {
		rejections, err = ParseRejections(existing)
		if err != nil {
			return fmt.Errorf("failed to parse existing rejections: %w", err)
		}
	}

	// Check if rejection already exists
	for i, rej := range rejections {
		if rej.Matches(child) {
			// Update existing rejection
			rejections[i].Reason = reason
			rejections[i].Generation = parentObj.GetGeneration()
			return a.updateRejections(ctx, parentObj, annotations, rejections)
		}
	}

	// Add new rejection
	rejection := Rejection{
		APIVersion: child.APIVersion,
		Kind:       child.Kind,
		Name:       child.Name,
		Reason:     reason,
		Generation: parentObj.GetGeneration(),
	}
	rejections = append(rejections, rejection)

	return a.updateRejections(ctx, parentObj, annotations, rejections)
}

// ApplySnooze sets the snooze-until annotation on the parent object.
func (a *ActionApplier) ApplySnooze(ctx context.Context, parent ObjectRef, duration time.Duration) error {
	// Fetch the parent object
	parentObj, err := a.fetchObject(ctx, parent)
	if err != nil {
		return fmt.Errorf("failed to fetch parent: %w", err)
	}

	// Get current annotations
	annotations := parentObj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// Set snooze-until
	snoozeUntil := time.Now().Add(duration).UTC().Format(time.RFC3339)
	annotations[SnoozeAnnotation] = snoozeUntil

	parentObj.SetAnnotations(annotations)
	if err := a.client.Update(ctx, parentObj); err != nil {
		return fmt.Errorf("failed to update parent: %w", err)
	}

	return nil
}

// RemoveApproval removes an approval for a specific child from the parent.
func (a *ActionApplier) RemoveApproval(ctx context.Context, parent ObjectRef, child ChildRef) error {
	// Fetch the parent object
	parentObj, err := a.fetchObject(ctx, parent)
	if err != nil {
		return fmt.Errorf("failed to fetch parent: %w", err)
	}

	annotations := parentObj.GetAnnotations()
	if annotations == nil {
		return nil // No annotations, nothing to remove
	}

	existing := annotations[ApprovalsAnnotation]
	if existing == "" {
		return nil // No approvals
	}

	approvals, err := ParseApprovals(existing)
	if err != nil {
		return fmt.Errorf("failed to parse existing approvals: %w", err)
	}

	// Filter out the matching approval
	var filtered []Approval
	for _, app := range approvals {
		if !app.Matches(child) {
			filtered = append(filtered, app)
		}
	}

	if len(filtered) == len(approvals) {
		return nil // Nothing was removed
	}

	return a.updateApprovals(ctx, parentObj, annotations, filtered)
}

// RemoveRejection removes a rejection for a specific child from the parent.
func (a *ActionApplier) RemoveRejection(ctx context.Context, parent ObjectRef, child ChildRef) error {
	// Fetch the parent object
	parentObj, err := a.fetchObject(ctx, parent)
	if err != nil {
		return fmt.Errorf("failed to fetch parent: %w", err)
	}

	annotations := parentObj.GetAnnotations()
	if annotations == nil {
		return nil // No annotations, nothing to remove
	}

	existing := annotations[RejectionsAnnotation]
	if existing == "" {
		return nil // No rejections
	}

	rejections, err := ParseRejections(existing)
	if err != nil {
		return fmt.Errorf("failed to parse existing rejections: %w", err)
	}

	// Filter out the matching rejection
	var filtered []Rejection
	for _, rej := range rejections {
		if !rej.Matches(child) {
			filtered = append(filtered, rej)
		}
	}

	if len(filtered) == len(rejections) {
		return nil // Nothing was removed
	}

	return a.updateRejections(ctx, parentObj, annotations, filtered)
}

// ClearSnooze removes the snooze annotation from the parent.
func (a *ActionApplier) ClearSnooze(ctx context.Context, parent ObjectRef) error {
	// Fetch the parent object
	parentObj, err := a.fetchObject(ctx, parent)
	if err != nil {
		return fmt.Errorf("failed to fetch parent: %w", err)
	}

	annotations := parentObj.GetAnnotations()
	if annotations == nil || annotations[SnoozeAnnotation] == "" {
		return nil // No snooze to clear
	}

	delete(annotations, SnoozeAnnotation)
	parentObj.SetAnnotations(annotations)

	if err := a.client.Update(ctx, parentObj); err != nil {
		return fmt.Errorf("failed to update parent: %w", err)
	}

	return nil
}

// fetchObject fetches an object by reference.
func (a *ActionApplier) fetchObject(ctx context.Context, ref ObjectRef) (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid API version: %w", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gv.WithKind(ref.Kind))

	key := client.ObjectKey{
		Namespace: ref.Namespace,
		Name:      ref.Name,
	}

	if err := a.client.Get(ctx, key, obj); err != nil {
		return nil, err
	}

	return obj, nil
}

// updateApprovals updates the approvals annotation on the object.
func (a *ActionApplier) updateApprovals(ctx context.Context, obj *unstructured.Unstructured, annotations map[string]string, approvals []Approval) error {
	if len(approvals) == 0 {
		delete(annotations, ApprovalsAnnotation)
	} else {
		data, err := json.Marshal(approvals)
		if err != nil {
			return fmt.Errorf("failed to marshal approvals: %w", err)
		}
		annotations[ApprovalsAnnotation] = string(data)
	}

	obj.SetAnnotations(annotations)
	if err := a.client.Update(ctx, obj); err != nil {
		return fmt.Errorf("failed to update object: %w", err)
	}

	return nil
}

// updateRejections updates the rejections annotation on the object.
func (a *ActionApplier) updateRejections(ctx context.Context, obj *unstructured.Unstructured, annotations map[string]string, rejections []Rejection) error {
	if len(rejections) == 0 {
		delete(annotations, RejectionsAnnotation)
	} else {
		data, err := json.Marshal(rejections)
		if err != nil {
			return fmt.Errorf("failed to marshal rejections: %w", err)
		}
		annotations[RejectionsAnnotation] = string(data)
	}

	obj.SetAnnotations(annotations)
	if err := a.client.Update(ctx, obj); err != nil {
		return fmt.Errorf("failed to update object: %w", err)
	}

	return nil
}

// MarshalRejections marshals rejections to JSON for annotation.
func MarshalRejections(rejections []Rejection) (string, error) {
	if len(rejections) == 0 {
		return "", nil
	}
	data, err := json.Marshal(rejections)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
