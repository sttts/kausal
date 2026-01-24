//go:build e2e

package kubernetes

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

// =============================================================================
// Setup Verification Tests
// =============================================================================

// TestKausalityPodsReady verifies that the kausality webhook pods are running and ready.
// This is a prerequisite for all other tests.
func TestKausalityPodsReady(t *testing.T) {
	ctx := context.Background()

	t.Log("Checking that kausality webhook pods are running...")

	ktesting.Eventually(t, func() (bool, string) {
		pods, err := clientset.CoreV1().Pods(kausalityNS).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=kausality",
		})
		if err != nil {
			return false, fmt.Sprintf("error listing pods: %v", err)
		}
		if len(pods.Items) == 0 {
			return false, "no kausality pods found yet"
		}

		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				return false, fmt.Sprintf("pod %s phase=%s, waiting for Running", pod.Name, pod.Status.Phase)
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status != corev1.ConditionTrue {
					return false, fmt.Sprintf("pod %s not ready yet", pod.Name)
				}
			}
		}
		return true, fmt.Sprintf("all %d kausality pods are ready", len(pods.Items))
	}, defaultTimeout, defaultInterval, "kausality pods should be ready")

	t.Log("All kausality webhook pods are running and ready")
}

// TestWebhookConfiguration verifies that the MutatingWebhookConfiguration is properly set up.
func TestWebhookConfiguration(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Webhook Configuration ===")
	t.Log("Verifying that the MutatingWebhookConfiguration is correctly installed.")

	t.Log("")
	t.Log("Fetching MutatingWebhookConfiguration 'kausality'...")
	webhooks, err := clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		ctx, "kausality", metav1.GetOptions{},
	)
	require.NoError(t, err, "MutatingWebhookConfiguration should exist")
	require.NotEmpty(t, webhooks.Webhooks, "should have at least one webhook")

	webhook := webhooks.Webhooks[0]
	t.Logf("Found webhook: %s", webhook.Name)

	t.Log("")
	t.Log("Verifying webhook properties...")

	assert.Equal(t, "mutating.webhook.kausality.io", webhook.Name)
	t.Log("  - Webhook name is correct: mutating.webhook.kausality.io")

	assert.NotEmpty(t, webhook.ClientConfig.CABundle, "CA bundle should be configured")
	t.Logf("  - CA bundle is configured (%d bytes)", len(webhook.ClientConfig.CABundle))

	assert.NotEmpty(t, webhook.Rules, "webhook should have rules")
	t.Logf("  - Found %d rule(s)", len(webhook.Rules))

	// Verify it intercepts apps resources
	t.Log("")
	t.Log("Checking that webhook intercepts apps/* resources...")
	foundApps := false
	for _, rule := range webhook.Rules {
		t.Logf("  Rule: apiGroups=%v, resources=%v", rule.APIGroups, rule.Resources)
		for _, group := range rule.APIGroups {
			if group == "apps" {
				foundApps = true
			}
		}
	}
	assert.True(t, foundApps, "webhook should intercept apps/* resources")

	t.Log("")
	t.Log("SUCCESS: Webhook configuration is correct")
}
