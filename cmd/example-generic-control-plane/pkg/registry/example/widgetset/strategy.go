// Package widgetset provides REST storage for WidgetSet resources.
package widgetset

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"

	examplev1alpha1 "github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/apis/example/v1alpha1"
)

// Strategy implements behavior for WidgetSet resources.
type Strategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// NewStrategy creates a new WidgetSet strategy.
func NewStrategy(typer runtime.ObjectTyper) Strategy {
	return Strategy{typer, names.SimpleNameGenerator}
}

// NamespaceScoped returns true because WidgetSets are namespaced.
func (Strategy) NamespaceScoped() bool {
	return true
}

// PrepareForCreate clears status before creation.
func (Strategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	widgetSet := obj.(*examplev1alpha1.WidgetSet)
	widgetSet.Status = examplev1alpha1.WidgetSetStatus{}
}

// PrepareForUpdate preserves status on update.
func (Strategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newWidgetSet := obj.(*examplev1alpha1.WidgetSet)
	oldWidgetSet := old.(*examplev1alpha1.WidgetSet)
	// Preserve status - it's updated via the status subresource
	newWidgetSet.Status = oldWidgetSet.Status
}

// Validate validates a new WidgetSet.
func (Strategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	widgetSet := obj.(*examplev1alpha1.WidgetSet)
	return validateWidgetSet(widgetSet)
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (Strategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

// AllowCreateOnUpdate returns false because WidgetSets are created via POST.
func (Strategy) AllowCreateOnUpdate() bool {
	return false
}

// ValidateUpdate validates an update to an existing WidgetSet.
func (Strategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	widgetSet := obj.(*examplev1alpha1.WidgetSet)
	return validateWidgetSet(widgetSet)
}

// WarningsOnUpdate returns warnings for the given update.
func (Strategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}

// AllowUnconditionalUpdate allows unconditional updates.
func (Strategy) AllowUnconditionalUpdate() bool {
	return true
}

// Canonicalize normalizes the object after validation.
func (Strategy) Canonicalize(obj runtime.Object) {
}

func validateWidgetSet(widgetSet *examplev1alpha1.WidgetSet) field.ErrorList {
	allErrs := field.ErrorList{}
	if widgetSet.Spec.Replicas < 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "replicas"), widgetSet.Spec.Replicas, "must be non-negative"))
	}
	return allErrs
}

// StatusStrategy implements behavior for WidgetSet status updates.
type StatusStrategy struct {
	Strategy
}

// NewStatusStrategy creates a new WidgetSet status strategy.
func NewStatusStrategy(strategy Strategy) StatusStrategy {
	return StatusStrategy{strategy}
}

// PrepareForUpdate preserves spec on status update.
func (StatusStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newWidgetSet := obj.(*examplev1alpha1.WidgetSet)
	oldWidgetSet := old.(*examplev1alpha1.WidgetSet)
	// Preserve spec - only status changes on status update
	newWidgetSet.Spec = oldWidgetSet.Spec
}

// ValidateUpdate validates a status update.
func (StatusStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return field.ErrorList{}
}

// GetAttrs returns labels and fields of a WidgetSet for filtering.
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	widgetSet, ok := obj.(*examplev1alpha1.WidgetSet)
	if !ok {
		return nil, nil, fmt.Errorf("not a WidgetSet")
	}
	return widgetSet.Labels, SelectableFields(widgetSet), nil
}

// SelectableFields returns the fields that can be used in field selectors.
func SelectableFields(obj *examplev1alpha1.WidgetSet) fields.Set {
	return generic.ObjectMetaFieldsSet(&obj.ObjectMeta, true)
}

// MatchWidgetSet returns a generic matcher for a WidgetSet.
func MatchWidgetSet(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label:    label,
		Field:    field,
		GetAttrs: GetAttrs,
	}
}

// Ensure strategies implement the required interfaces.
var _ rest.RESTCreateStrategy = Strategy{}
var _ rest.RESTUpdateStrategy = Strategy{}
var _ rest.RESTUpdateStrategy = StatusStrategy{}
