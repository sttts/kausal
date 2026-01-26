// Package widget provides REST storage for Widget resources.
package widget

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

// Strategy implements behavior for Widget resources.
type Strategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// NewStrategy creates a new Widget strategy.
func NewStrategy(typer runtime.ObjectTyper) Strategy {
	return Strategy{typer, names.SimpleNameGenerator}
}

// NamespaceScoped returns true because Widgets are namespaced.
func (Strategy) NamespaceScoped() bool {
	return true
}

// PrepareForCreate clears status before creation.
func (Strategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	// Widget has no status to clear
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (Strategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	// Widget has no status to preserve
}

// Validate validates a new Widget.
func (Strategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	widget := obj.(*examplev1alpha1.Widget)
	return validateWidget(widget)
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (Strategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}

// AllowCreateOnUpdate returns false because Widgets are created via POST.
func (Strategy) AllowCreateOnUpdate() bool {
	return false
}

// ValidateUpdate validates an update to an existing Widget.
func (Strategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	widget := obj.(*examplev1alpha1.Widget)
	return validateWidget(widget)
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

func validateWidget(widget *examplev1alpha1.Widget) field.ErrorList {
	allErrs := field.ErrorList{}
	// Add validation if needed
	return allErrs
}

// GetAttrs returns labels and fields of a Widget for filtering.
func GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	widget, ok := obj.(*examplev1alpha1.Widget)
	if !ok {
		return nil, nil, fmt.Errorf("not a Widget")
	}
	return widget.Labels, SelectableFields(widget), nil
}

// SelectableFields returns the fields that can be used in field selectors.
func SelectableFields(obj *examplev1alpha1.Widget) fields.Set {
	return generic.ObjectMetaFieldsSet(&obj.ObjectMeta, true)
}

// MatchWidget returns a generic matcher for a Widget.
func MatchWidget(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label:    label,
		Field:    field,
		GetAttrs: GetAttrs,
	}
}

// Ensure Strategy implements the required interfaces.
var _ rest.RESTCreateStrategy = Strategy{}
var _ rest.RESTUpdateStrategy = Strategy{}
