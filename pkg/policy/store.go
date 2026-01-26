package policy

import (
	"context"
	"sort"
	"sync"

	"github.com/go-logr/logr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
)

// Store caches Kausality policies and resolves modes for resources.
type Store struct {
	client   client.Client
	log      logr.Logger
	mu       sync.RWMutex
	policies []kausalityv1alpha1.Kausality
}

// NewStore creates a new policy store.
func NewStore(c client.Client, log logr.Logger) *Store {
	return &Store{
		client: c,
		log:    log.WithName("policy-store"),
	}
}

// Refresh reloads all Kausality policies from the API server.
func (s *Store) Refresh(ctx context.Context) error {
	var list kausalityv1alpha1.KausalityList
	if err := s.client.List(ctx, &list); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Filter out deleting policies and sort by name for determinism
	s.policies = make([]kausalityv1alpha1.Kausality, 0, len(list.Items))
	for _, p := range list.Items {
		if p.DeletionTimestamp.IsZero() {
			s.policies = append(s.policies, p)
		}
	}
	sort.Slice(s.policies, func(i, j int) bool {
		return s.policies[i].Name < s.policies[j].Name
	})

	s.log.V(1).Info("refreshed policies", "count", len(s.policies))
	return nil
}

// ResourceContext provides context for mode resolution.
type ResourceContext struct {
	// GVR identifies the resource type.
	GVR schema.GroupVersionResource

	// Namespace is the object's namespace (empty for cluster-scoped).
	Namespace string

	// NamespaceLabels are the labels on the namespace.
	NamespaceLabels map[string]string

	// ObjectLabels are the labels on the object.
	ObjectLabels map[string]string
}

// ModeAnnotation is the annotation key for runtime mode override.
const ModeAnnotation = "kausality.io/mode"

// ResolveMode returns the drift detection mode for a resource.
// Precedence: object annotation > namespace annotation > CRD policy > default (log).
func (s *Store) ResolveMode(ctx ResourceContext, objectAnnotations, namespaceAnnotations map[string]string) kausalityv1alpha1.Mode {
	// 1. Check object annotation
	if mode := objectAnnotations[ModeAnnotation]; isValidMode(mode) {
		return kausalityv1alpha1.Mode(mode)
	}

	// 2. Check namespace annotation
	if mode := namespaceAnnotations[ModeAnnotation]; isValidMode(mode) {
		return kausalityv1alpha1.Mode(mode)
	}

	// 3. Find matching policy with highest specificity
	s.mu.RLock()
	defer s.mu.RUnlock()

	var bestPolicy *kausalityv1alpha1.Kausality
	var bestSpecificity int

	for i := range s.policies {
		policy := &s.policies[i]
		if !s.policyMatches(policy, ctx) {
			continue
		}

		specificity := s.calculateSpecificity(policy, ctx)
		if bestPolicy == nil || specificity > bestSpecificity {
			bestPolicy = policy
			bestSpecificity = specificity
		}
	}

	if bestPolicy == nil {
		// No matching policy - default to log
		return kausalityv1alpha1.ModeLog
	}

	// 4. Check overrides within the matching policy
	mode := s.resolveOverrides(bestPolicy, ctx)
	return mode
}

// IsTracked returns true if the resource is tracked by any Kausality policy.
func (s *Store) IsTracked(ctx ResourceContext) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := range s.policies {
		if s.policyMatches(&s.policies[i], ctx) {
			return true
		}
	}
	return false
}

// policyMatches checks if a policy matches the resource context.
func (s *Store) policyMatches(policy *kausalityv1alpha1.Kausality, ctx ResourceContext) bool {
	// Check resources
	if !s.resourcesMatch(policy.Spec.Resources, ctx.GVR) {
		return false
	}

	// Check namespaces
	if !s.namespacesMatch(policy.Spec.Namespaces, ctx.Namespace, ctx.NamespaceLabels) {
		return false
	}

	// Check object selector
	if !s.objectSelectorMatches(policy.Spec.ObjectSelector, ctx.ObjectLabels) {
		return false
	}

	return true
}

// resourcesMatch checks if any resource rule matches.
func (s *Store) resourcesMatch(rules []kausalityv1alpha1.ResourceRule, gvr schema.GroupVersionResource) bool {
	for _, rule := range rules {
		if s.ruleMatches(rule, gvr) {
			return true
		}
	}
	return false
}

// ruleMatches checks if a single resource rule matches.
func (s *Store) ruleMatches(rule kausalityv1alpha1.ResourceRule, gvr schema.GroupVersionResource) bool {
	// Check API group
	groupMatches := false
	for _, g := range rule.APIGroups {
		if g == gvr.Group {
			groupMatches = true
			break
		}
	}
	if !groupMatches {
		return false
	}

	// Check resource
	resourceMatches := false
	for _, r := range rule.Resources {
		if r == "*" || r == gvr.Resource {
			resourceMatches = true
			break
		}
	}
	if !resourceMatches {
		return false
	}

	// Check exclusions
	for _, e := range rule.Excluded {
		if e == gvr.Resource {
			return false
		}
	}

	return true
}

// namespacesMatch checks if the namespace matches the selector.
func (s *Store) namespacesMatch(selector *kausalityv1alpha1.NamespaceSelector, namespace string, nsLabels map[string]string) bool {
	// No selector = all namespaces
	if selector == nil {
		return true
	}

	// Check exclusions first
	for _, excluded := range selector.Excluded {
		if excluded == namespace {
			return false
		}
	}

	// Check explicit names
	if len(selector.Names) > 0 {
		for _, name := range selector.Names {
			if name == namespace {
				return true
			}
		}
		return false
	}

	// Check label selector
	if selector.Selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(selector.Selector)
		if err != nil {
			return false
		}
		return sel.Matches(labels.Set(nsLabels))
	}

	return true
}

// objectSelectorMatches checks if the object labels match the selector.
func (s *Store) objectSelectorMatches(selector *metav1.LabelSelector, objLabels map[string]string) bool {
	if selector == nil {
		return true
	}

	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false
	}
	return sel.Matches(labels.Set(objLabels))
}

// calculateSpecificity returns a score for policy specificity.
// Higher score = more specific = wins in conflicts.
func (s *Store) calculateSpecificity(policy *kausalityv1alpha1.Kausality, ctx ResourceContext) int {
	score := 0

	// Namespace specificity: explicit names > selector > all
	if policy.Spec.Namespaces != nil {
		if len(policy.Spec.Namespaces.Names) > 0 {
			score += 100 // Explicit namespace names
		} else if policy.Spec.Namespaces.Selector != nil {
			score += 50 // Namespace selector
		}
	}
	// No namespace selector = 0 (matches all)

	// Resource specificity: explicit resources > wildcard
	for _, rule := range policy.Spec.Resources {
		for _, g := range rule.APIGroups {
			if g == ctx.GVR.Group {
				for _, r := range rule.Resources {
					if r == ctx.GVR.Resource {
						score += 10 // Explicit resource
					}
					// Wildcard "*" = 0
				}
			}
		}
	}

	// Object selector adds specificity
	if policy.Spec.ObjectSelector != nil {
		score += 5
	}

	return score
}

// resolveOverrides finds the applicable mode from policy overrides.
func (s *Store) resolveOverrides(policy *kausalityv1alpha1.Kausality, ctx ResourceContext) kausalityv1alpha1.Mode {
	// Evaluate overrides in order; first match wins
	for _, override := range policy.Spec.Overrides {
		if s.overrideMatches(override, ctx) {
			return override.Mode
		}
	}

	return policy.Spec.Mode
}

// overrideMatches checks if an override applies to the context.
func (s *Store) overrideMatches(override kausalityv1alpha1.ModeOverride, ctx ResourceContext) bool {
	// Check API groups (if specified)
	if len(override.APIGroups) > 0 {
		matches := false
		for _, g := range override.APIGroups {
			if g == ctx.GVR.Group {
				matches = true
				break
			}
		}
		if !matches {
			return false
		}
	}

	// Check resources (if specified)
	if len(override.Resources) > 0 {
		matches := false
		for _, r := range override.Resources {
			if r == ctx.GVR.Resource {
				matches = true
				break
			}
		}
		if !matches {
			return false
		}
	}

	// Check namespaces (if specified)
	if len(override.Namespaces) > 0 {
		matches := false
		for _, ns := range override.Namespaces {
			if ns == ctx.Namespace {
				matches = true
				break
			}
		}
		if !matches {
			return false
		}
	}

	return true
}

// isValidMode checks if a mode string is valid.
func isValidMode(mode string) bool {
	return mode == string(kausalityv1alpha1.ModeLog) || mode == string(kausalityv1alpha1.ModeEnforce)
}
