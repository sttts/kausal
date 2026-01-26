package policy

import (
	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
)

// Resolver resolves drift detection mode for resources.
// This interface allows different implementations:
// - Store: CRD-based policy resolution with controller-runtime client
// - StaticResolver: In-memory policy for embedded apiservers
type Resolver interface {
	// ResolveMode returns the drift detection mode for a resource.
	// Precedence: object annotation > namespace annotation > policy > default (log).
	ResolveMode(ctx ResourceContext, objectAnnotations, namespaceAnnotations map[string]string) kausalityv1alpha1.Mode

	// IsTracked returns true if the resource is tracked by any policy.
	IsTracked(ctx ResourceContext) bool
}

// StaticResolver provides a fixed mode for all resources.
// Useful for embedded apiservers that don't need dynamic policy configuration.
type StaticResolver struct {
	Mode kausalityv1alpha1.Mode
}

// NewStaticResolver creates a resolver that always returns the specified mode.
func NewStaticResolver(mode kausalityv1alpha1.Mode) *StaticResolver {
	return &StaticResolver{Mode: mode}
}

// ResolveMode returns the configured static mode, unless overridden by annotations.
func (r *StaticResolver) ResolveMode(ctx ResourceContext, objectAnnotations, namespaceAnnotations map[string]string) kausalityv1alpha1.Mode {
	// Check object annotation
	if mode := objectAnnotations[ModeAnnotation]; isValidMode(mode) {
		return kausalityv1alpha1.Mode(mode)
	}

	// Check namespace annotation
	if mode := namespaceAnnotations[ModeAnnotation]; isValidMode(mode) {
		return kausalityv1alpha1.Mode(mode)
	}

	return r.Mode
}

// IsTracked always returns true - static resolver tracks everything.
func (r *StaticResolver) IsTracked(ctx ResourceContext) bool {
	return true
}
