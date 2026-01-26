// Package policy implements the Kausality policy controller.
package policy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
)

const (
	// FinalizerName is the finalizer used by the controller.
	FinalizerName = "kausality.io/policy-controller"

	// WebhookName is the name of the MutatingWebhookConfiguration.
	WebhookName = "kausality"

	// ConditionTypeReady indicates the policy is ready.
	ConditionTypeReady = "Ready"

	// ConditionTypeWebhookConfigured indicates webhook rules are applied.
	ConditionTypeWebhookConfigured = "WebhookConfigured"

	// AggregationLabel is the label used for RBAC aggregation.
	// ClusterRoles with this label are aggregated into the webhook-resources role.
	AggregationLabel = "kausality.io/aggregate-to-webhook-resources"

	// ClusterRolePrefix is the prefix for generated per-policy ClusterRoles.
	ClusterRolePrefix = "kausality-policy-"

	// ManagedByLabel indicates a resource is managed by kausality.
	ManagedByLabel = "app.kubernetes.io/managed-by"

	// PolicyNameLabel indicates which policy owns the ClusterRole.
	PolicyNameLabel = "kausality.io/policy"
)

// Controller reconciles Kausality resources.
type Controller struct {
	client.Client
	Log             logr.Logger
	Scheme          *runtime.Scheme
	DiscoveryClient discovery.DiscoveryInterface

	// WebhookName is the name of the MutatingWebhookConfiguration to manage.
	WebhookName string

	// WebhookServiceRef identifies the webhook service.
	WebhookServiceRef WebhookServiceRef

	// ExcludedNamespaces are namespaces to exclude from webhook rules.
	ExcludedNamespaces []string
}

// WebhookServiceRef identifies the webhook service.
type WebhookServiceRef struct {
	Namespace string
	Name      string
	Port      int32
	Path      string
}

// Reconcile handles a single Kausality resource reconciliation.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := c.Log.WithValues("kausality", req.Name)

	// Fetch the Kausality instance
	var policy kausalityv1alpha1.Kausality
	if err := c.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !policy.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&policy, FinalizerName) {
			// ClusterRole is cleaned up by Kubernetes GC via owner reference

			// Reconcile webhook to remove this policy's rules
			if err := c.reconcileWebhook(ctx, log); err != nil {
				return requeueOnConflict(err)
			}

			// Remove finalizer
			controllerutil.RemoveFinalizer(&policy, FinalizerName)
			if err := c.Update(ctx, &policy); err != nil {
				return requeueOnConflict(err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if missing
	if !controllerutil.ContainsFinalizer(&policy, FinalizerName) {
		controllerutil.AddFinalizer(&policy, FinalizerName)
		if err := c.Update(ctx, &policy); err != nil {
			return requeueOnConflict(err)
		}
	}

	// Reconcile the webhook configuration
	if err := c.reconcileWebhook(ctx, log); err != nil {
		c.setCondition(&policy, ConditionTypeWebhookConfigured, metav1.ConditionFalse, "ReconcileFailed", err.Error())
		c.setCondition(&policy, ConditionTypeReady, metav1.ConditionFalse, "WebhookNotConfigured", "Webhook configuration failed")
		if statusErr := c.Status().Update(ctx, &policy); statusErr != nil {
			if !apierrors.IsConflict(statusErr) {
				log.Error(statusErr, "failed to update status")
			}
		}
		return requeueOnConflict(err)
	}

	// Reconcile RBAC for this policy
	if err := c.reconcileClusterRole(ctx, log, &policy); err != nil {
		if !apierrors.IsConflict(err) {
			log.Error(err, "failed to reconcile RBAC")
		}
		// Don't fail the whole reconciliation for RBAC errors, but requeue on conflict
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Update status
	c.setCondition(&policy, ConditionTypeWebhookConfigured, metav1.ConditionTrue, "RulesApplied", "Webhook rules updated")
	c.setCondition(&policy, ConditionTypeReady, metav1.ConditionTrue, "Reconciled", "Policy is active")
	if err := c.Status().Update(ctx, &policy); err != nil {
		return requeueOnConflict(err)
	}

	return ctrl.Result{}, nil
}

// requeueOnConflict returns a requeue result without error for conflict errors,
// allowing silent retry. Other errors are returned normally.
func requeueOnConflict(err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, err
}

// reconcileWebhook updates the MutatingWebhookConfiguration based on all Kausality policies.
func (c *Controller) reconcileWebhook(ctx context.Context, log logr.Logger) error {
	// List all Kausality policies
	var policies kausalityv1alpha1.KausalityList
	if err := c.List(ctx, &policies); err != nil {
		return fmt.Errorf("failed to list policies: %w", err)
	}

	// Aggregate rules from all policies
	rules, err := c.aggregateRules(policies.Items)
	if err != nil {
		return fmt.Errorf("failed to aggregate rules: %w", err)
	}

	log.Info("aggregated webhook rules", "ruleCount", len(rules), "policyCount", len(policies.Items))

	// Get or create the webhook configuration
	var webhook admissionregistrationv1.MutatingWebhookConfiguration
	webhookKey := client.ObjectKey{Name: c.WebhookName}
	if err := c.Get(ctx, webhookKey, &webhook); err != nil {
		return fmt.Errorf("failed to get webhook configuration %q: %w", c.WebhookName, err)
	}

	// Update the webhook rules
	if len(webhook.Webhooks) == 0 {
		return fmt.Errorf("webhook configuration %q has no webhooks defined", c.WebhookName)
	}

	// Update the first webhook's rules
	webhook.Webhooks[0].Rules = rules
	webhook.Webhooks[0].NamespaceSelector = c.buildNamespaceSelector()

	if err := c.Update(ctx, &webhook); err != nil {
		return fmt.Errorf("failed to update webhook configuration: %w", err)
	}

	return nil
}

// aggregateRules builds webhook rules from all Kausality policies.
func (c *Controller) aggregateRules(policies []kausalityv1alpha1.Kausality) ([]admissionregistrationv1.RuleWithOperations, error) {
	// Collect all resource rules, deduplicating by apiGroup+resource
	type resourceKey struct {
		apiGroup string
		resource string
	}
	seen := make(map[resourceKey]bool)
	var allResources []resourceKey

	for _, policy := range policies {
		// Skip policies being deleted
		if !policy.DeletionTimestamp.IsZero() {
			continue
		}

		for _, rule := range policy.Spec.Resources {
			resources, err := c.expandResources(rule)
			if err != nil {
				return nil, fmt.Errorf("failed to expand resources for policy %q: %w", policy.Name, err)
			}

			for _, apiGroup := range rule.APIGroups {
				for _, resource := range resources {
					key := resourceKey{apiGroup: apiGroup, resource: resource}
					if !seen[key] {
						seen[key] = true
						allResources = append(allResources, key)
					}
				}
			}
		}
	}

	// Group resources by apiGroup for efficient webhook rules
	groupedResources := make(map[string][]string)
	for _, res := range allResources {
		groupedResources[res.apiGroup] = append(groupedResources[res.apiGroup], res.resource)
	}

	// Sort for deterministic output
	var apiGroups []string
	for g := range groupedResources {
		apiGroups = append(apiGroups, g)
	}
	sort.Strings(apiGroups)

	// Build webhook rules
	var rules []admissionregistrationv1.RuleWithOperations
	fail := admissionregistrationv1.Fail
	allScopes := admissionregistrationv1.AllScopes

	for _, apiGroup := range apiGroups {
		resources := groupedResources[apiGroup]
		sort.Strings(resources)

		// Spec changes rule (CREATE, UPDATE, DELETE)
		rules = append(rules, admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{
				admissionregistrationv1.Create,
				admissionregistrationv1.Update,
				admissionregistrationv1.Delete,
			},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{apiGroup},
				APIVersions: []string{"*"},
				Resources:   resources,
				Scope:       &allScopes,
			},
		})

		// Status subresource rule (UPDATE only) - for controller identification
		var statusResources []string
		for _, r := range resources {
			statusResources = append(statusResources, r+"/status")
		}
		rules = append(rules, admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{
				admissionregistrationv1.Update,
			},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{apiGroup},
				APIVersions: []string{"*"},
				Resources:   statusResources,
				Scope:       &allScopes,
			},
		})
	}

	_ = fail // Will be used when we configure failurePolicy
	return rules, nil
}

// expandResources expands a ResourceRule, resolving "*" via discovery.
func (c *Controller) expandResources(rule kausalityv1alpha1.ResourceRule) ([]string, error) {
	// Check if we need to expand wildcards
	hasWildcard := false
	for _, r := range rule.Resources {
		if r == "*" {
			hasWildcard = true
			break
		}
	}

	if !hasWildcard {
		// No wildcard, return as-is (minus excluded)
		return filterExcluded(rule.Resources, rule.Excluded), nil
	}

	// Expand wildcard via discovery
	var allResources []string
	for _, apiGroup := range rule.APIGroups {
		resources, err := c.discoverResources(apiGroup)
		if err != nil {
			return nil, fmt.Errorf("failed to discover resources for group %q: %w", apiGroup, err)
		}
		allResources = append(allResources, resources...)
	}

	return filterExcluded(allResources, rule.Excluded), nil
}

// discoverResources returns all resources for an API group.
func (c *Controller) discoverResources(apiGroup string) ([]string, error) {
	// Get all API resources for the group
	var resources []string

	// Get server groups and resources
	_, apiResourceLists, err := c.DiscoveryClient.ServerGroupsAndResources()
	if err != nil {
		// Discovery can return partial results with errors
		if apiResourceLists == nil {
			return nil, fmt.Errorf("discovery failed: %w", err)
		}
	}

	for _, resourceList := range apiResourceLists {
		// Parse the GroupVersion
		gv := resourceList.GroupVersion
		group := ""
		if idx := strings.Index(gv, "/"); idx >= 0 {
			group = gv[:idx]
		}

		if group != apiGroup {
			continue
		}

		for _, r := range resourceList.APIResources {
			// Skip subresources (they contain "/")
			if strings.Contains(r.Name, "/") {
				continue
			}
			resources = append(resources, r.Name)
		}
	}

	// Deduplicate (same resource might appear in multiple versions)
	seen := make(map[string]bool)
	var unique []string
	for _, r := range resources {
		if !seen[r] {
			seen[r] = true
			unique = append(unique, r)
		}
	}

	return unique, nil
}

// buildNamespaceSelector builds the namespace selector for the webhook.
func (c *Controller) buildNamespaceSelector() *metav1.LabelSelector {
	if len(c.ExcludedNamespaces) == 0 {
		return nil
	}

	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   c.ExcludedNamespaces,
			},
		},
	}
}

// setCondition sets a condition on the Kausality resource.
func (c *Controller) setCondition(policy *kausalityv1alpha1.Kausality, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()

	// Find existing condition
	for i := range policy.Status.Conditions {
		if policy.Status.Conditions[i].Type == condType {
			// Only update if changed
			if policy.Status.Conditions[i].Status != status ||
				policy.Status.Conditions[i].Reason != reason ||
				policy.Status.Conditions[i].Message != message {
				policy.Status.Conditions[i].Status = status
				policy.Status.Conditions[i].Reason = reason
				policy.Status.Conditions[i].Message = message
				policy.Status.Conditions[i].LastTransitionTime = now
				policy.Status.Conditions[i].ObservedGeneration = policy.Generation
			}
			return
		}
	}

	// Add new condition
	policy.Status.Conditions = append(policy.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: policy.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kausalityv1alpha1.Kausality{}).
		Complete(c)
}

// reconcileClusterRole creates or updates a ClusterRole for the policy.
// The ClusterRole grants read/write access to resources defined in the policy
// and is labeled for aggregation into the main resource-access role.
func (c *Controller) reconcileClusterRole(ctx context.Context, log logr.Logger, policy *kausalityv1alpha1.Kausality) error {
	roleName := ClusterRolePrefix + policy.Name

	// Build RBAC rules from policy resources
	rules, err := c.buildRBACRules(policy)
	if err != nil {
		return fmt.Errorf("failed to build RBAC rules: %w", err)
	}

	// Try to get existing ClusterRole
	var existing rbacv1.ClusterRole
	err = c.Get(ctx, client.ObjectKey{Name: roleName}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get ClusterRole: %w", err)
	}

	desired := rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: roleName,
			Labels: map[string]string{
				AggregationLabel: "true",
				ManagedByLabel:   "kausality",
				PolicyNameLabel:  policy.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: kausalityv1alpha1.GroupVersion.String(),
					Kind:       "Kausality",
					Name:       policy.Name,
					UID:        policy.UID,
				},
			},
		},
		Rules: rules,
	}

	if apierrors.IsNotFound(err) {
		// Create new ClusterRole
		if err := c.Create(ctx, &desired); err != nil {
			return fmt.Errorf("failed to create ClusterRole: %w", err)
		}
		log.Info("created ClusterRole", "name", roleName, "rules", len(rules))
	} else {
		// Update existing ClusterRole
		existing.Labels = desired.Labels
		existing.Rules = desired.Rules
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("failed to update ClusterRole: %w", err)
		}
		log.Info("updated ClusterRole", "name", roleName, "rules", len(rules))
	}

	return nil
}

// buildRBACRules builds RBAC PolicyRules from a Kausality policy.
func (c *Controller) buildRBACRules(policy *kausalityv1alpha1.Kausality) ([]rbacv1.PolicyRule, error) {
	// Collect resources by API group
	groupedResources := make(map[string][]string)

	for _, rule := range policy.Spec.Resources {
		resources, err := c.expandResources(rule)
		if err != nil {
			return nil, fmt.Errorf("failed to expand resources: %w", err)
		}

		for _, apiGroup := range rule.APIGroups {
			groupedResources[apiGroup] = append(groupedResources[apiGroup], resources...)
		}
	}

	// Deduplicate resources per group
	for group, resources := range groupedResources {
		seen := make(map[string]bool)
		var unique []string
		for _, r := range resources {
			if !seen[r] {
				seen[r] = true
				unique = append(unique, r)
			}
		}
		sort.Strings(unique)
		groupedResources[group] = unique
	}

	// Sort API groups for deterministic output
	var apiGroups []string
	for g := range groupedResources {
		apiGroups = append(apiGroups, g)
	}
	sort.Strings(apiGroups)

	// Build rules - one per API group with read and write verbs
	var rules []rbacv1.PolicyRule
	for _, apiGroup := range apiGroups {
		resources := groupedResources[apiGroup]

		// Read access (get, list, watch)
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{apiGroup},
			Resources: resources,
			Verbs:     []string{"get", "list", "watch"},
		})

		// Write access (update, patch) for annotation updates
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{apiGroup},
			Resources: resources,
			Verbs:     []string{"update", "patch"},
		})
	}

	return rules, nil
}

// filterExcluded removes excluded resources from a list.
func filterExcluded(resources, excluded []string) []string {
	if len(excluded) == 0 {
		return resources
	}

	excludeSet := make(map[string]bool)
	for _, e := range excluded {
		excludeSet[e] = true
	}

	var filtered []string
	for _, r := range resources {
		if !excludeSet[r] {
			filtered = append(filtered, r)
		}
	}
	return filtered
}
