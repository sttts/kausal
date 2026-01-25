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

// ApplySnooze sets the snooze annotation on the parent object.
func (a *ActionApplier) ApplySnooze(ctx context.Context, parent ObjectRef, duration time.Duration, user, message string) error {
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

	// Create snooze with structured data
	snooze := &Snooze{
		Expiry:  time.Now().Add(duration).UTC(),
		User:    user,
		Message: message,
	}
	snoozeValue, err := MarshalSnooze(snooze)
	if err != nil {
		return fmt.Errorf("failed to marshal snooze: %w", err)
	}
	annotations[SnoozeAnnotation] = snoozeValue

	parentObj.SetAnnotations(annotations)
	if err := a.client.Update(ctx, parentObj); err != nil {
		return fmt.Errorf("failed to update parent: %w", err)
	}

	return nil
}

// ApplyFreeze sets the freeze annotation on the parent object.
func (a *ActionApplier) ApplyFreeze(ctx context.Context, parent ObjectRef, user, message string) error {
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

	// Create freeze with structured data
	freeze := &Freeze{
		User:    user,
		Message: message,
		At:      time.Now().UTC(),
	}
	freezeValue, err := MarshalFreeze(freeze)
	if err != nil {
		return fmt.Errorf("failed to marshal freeze: %w", err)
	}
	annotations[FreezeAnnotation] = freezeValue

	parentObj.SetAnnotations(annotations)
	if err := a.client.Update(ctx, parentObj); err != nil {
		return fmt.Errorf("failed to update parent: %w", err)
	}

	return nil
}

// ClearFreeze removes the freeze annotation from the parent.
func (a *ActionApplier) ClearFreeze(ctx context.Context, parent ObjectRef) error {
	// Fetch the parent object
	parentObj, err := a.fetchObject(ctx, parent)
	if err != nil {
		return fmt.Errorf("failed to fetch parent: %w", err)
	}

	annotations := parentObj.GetAnnotations()
	if annotations == nil || annotations[FreezeAnnotation] == "" {
		return nil // No freeze to clear
	}

	delete(annotations, FreezeAnnotation)
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
