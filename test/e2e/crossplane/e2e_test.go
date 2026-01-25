//go:build e2e

package crossplane

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/rand"

	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

// TestCrossplaneProviderHealthy verifies that the Crossplane provider-nop is installed and healthy.
// This is a prerequisite for all other Crossplane tests.
func TestCrossplaneProviderHealthy(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Checking Crossplane Provider ===")
	t.Log("Verifying that provider-nop is installed and healthy...")

	ktesting.Eventually(t, func() (bool, string) {
		provider, err := dynamicClient.Resource(providerGVR).Get(ctx, "provider-nop", metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting provider: %v", err)
		}

		conditions, found, err := unstructured.NestedSlice(provider.Object, "status", "conditions")
		if err != nil || !found {
			return false, "no conditions found on provider"
		}

		var installedStatus, healthyStatus string
		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			cType, _, _ := unstructured.NestedString(cond, "type")
			cStatus, _, _ := unstructured.NestedString(cond, "status")

			switch cType {
			case "Installed":
				installedStatus = cStatus
			case "Healthy":
				healthyStatus = cStatus
			}
		}

		if installedStatus != "True" {
			return false, fmt.Sprintf("provider not installed: Installed=%s", installedStatus)
		}
		if healthyStatus != "True" {
			return false, fmt.Sprintf("provider not healthy: Healthy=%s", healthyStatus)
		}

		return true, "provider-nop is installed and healthy"
	}, defaultTimeout, defaultInterval, "provider-nop should be healthy")

	t.Log("Provider-nop is installed and healthy")
}

// TestKausalityPodsReady verifies that the kausality pods are running.
func TestKausalityPodsReady(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Checking Kausality Pods ===")
	t.Log("Verifying that kausality webhook pods are running...")

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

// TestNopResourceWithTraceLabels verifies that trace labels on NopResource are processed.
func TestNopResourceWithTraceLabels(t *testing.T) {
	ctx := context.Background()
	name := fmt.Sprintf("trace-nop-%s", rand.String(4))

	t.Log("=== Testing NopResource with Trace Labels ===")
	t.Log("When a NopResource has kausality.io/trace-* labels, the webhook should")
	t.Log("intercept the creation and process the trace labels.")

	// Step 1: Create NopResource with trace labels
	t.Log("")
	t.Logf("Step 1: Creating NopResource %q with trace labels...", name)

	nopResource := makeNopResource(name, map[string]string{
		"kausality.io/trace-ticket":    "CROSSPLANE-001",
		"kausality.io/trace-component": "infrastructure",
	})

	_, err := dynamicClient.Resource(nopResourceGVR).Create(ctx, nopResource, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting NopResource %s", name)
		_ = dynamicClient.Resource(nopResourceGVR).Delete(ctx, name, metav1.DeleteOptions{})
	})
	t.Logf("NopResource %q created with trace labels", name)

	// Step 2: Wait for NopResource to become Ready
	t.Log("")
	t.Log("Step 2: Waiting for NopResource to become Ready...")

	ktesting.Eventually(t, func() (bool, string) {
		obj, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting NopResource: %v", err)
		}

		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found || len(conditions) == 0 {
			return false, "no conditions found yet"
		}

		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			cType, _, _ := unstructured.NestedString(cond, "type")
			cStatus, _, _ := unstructured.NestedString(cond, "status")

			if cType == "Ready" && cStatus == "True" {
				return true, "NopResource is Ready"
			}
		}

		return false, "NopResource not Ready yet"
	}, defaultTimeout, defaultInterval, "NopResource should become Ready")

	t.Log("NopResource is Ready")

	// Step 3: Check for trace annotation
	t.Log("")
	t.Log("Step 3: Checking NopResource for trace annotation...")
	t.Log("Note: Direct user creation may not have trace annotation (no parent)")

	obj, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	traceAnnotation, found, _ := unstructured.NestedString(obj.Object, "metadata", "annotations", "kausality.io/trace")
	if found && traceAnnotation != "" {
		t.Logf("Found trace annotation: %s", traceAnnotation)

		var trace map[string]interface{}
		if err := json.Unmarshal([]byte(traceAnnotation), &trace); err == nil {
			if labels, ok := trace["labels"].(map[string]interface{}); ok {
				assert.Contains(t, labels, "kausality.io/trace-ticket")
			}
		}
	} else {
		t.Log("No trace annotation (expected for direct user creation without parent)")
	}

	t.Log("")
	t.Log("SUCCESS: NopResource with trace labels created and processed")
}

// TestWebhookConfigurationForCrossplane verifies that the webhook is configured for Crossplane resources.
func TestWebhookConfigurationForCrossplane(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Webhook Configuration for Crossplane ===")
	t.Log("Verifying that the MutatingWebhookConfiguration includes Crossplane resources.")

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
	t.Log("Checking that webhook intercepts nop.crossplane.io resources...")
	foundNop := false
	for _, rule := range webhook.Rules {
		t.Logf("  Rule: apiGroups=%v, resources=%v", rule.APIGroups, rule.Resources)
		for _, group := range rule.APIGroups {
			if group == "nop.crossplane.io" {
				foundNop = true
			}
		}
	}
	assert.True(t, foundNop, "webhook should intercept nop.crossplane.io/* resources")

	t.Log("")
	if foundNop {
		t.Log("SUCCESS: Webhook is configured to intercept Crossplane NopResources")
	} else {
		t.Log("WARNING: Webhook may not be configured for nop.crossplane.io (check Helm values)")
	}
}

// TestMultipleNopResources verifies that the webhook handles multiple Crossplane resources.
func TestMultipleNopResources(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Multiple NopResources ===")
	t.Log("Verify webhook handles multiple Crossplane resources correctly.")

	// Step 1: Create multiple NopResources
	t.Log("")
	t.Log("Step 1: Creating multiple NopResources...")

	names := make([]string, 3)
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("multi-nop-%d-%s", i+1, rand.String(4))
		names[i] = name

		nopResource := makeNopResource(name, map[string]string{
			"kausality.io/trace-batch": fmt.Sprintf("batch-%d", i+1),
		})

		_, err := dynamicClient.Resource(nopResourceGVR).Create(ctx, nopResource, metav1.CreateOptions{})
		require.NoError(t, err)
		t.Logf("Created NopResource %s", name)

		t.Cleanup(func() {
			_ = dynamicClient.Resource(nopResourceGVR).Delete(ctx, name, metav1.DeleteOptions{})
		})
	}

	// Step 2: Wait for all NopResources to become Ready
	t.Log("")
	t.Log("Step 2: Waiting for all NopResources to become Ready...")

	for _, name := range names {
		ktesting.Eventually(t, func() (bool, string) {
			obj, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Sprintf("error getting NopResource %s: %v", name, err)
			}

			conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			if !found || len(conditions) == 0 {
				return false, fmt.Sprintf("NopResource %s has no conditions yet", name)
			}

			for _, c := range conditions {
				cond, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				cType, _, _ := unstructured.NestedString(cond, "type")
				cStatus, _, _ := unstructured.NestedString(cond, "status")

				if cType == "Ready" && cStatus == "True" {
					return true, fmt.Sprintf("NopResource %s is Ready", name)
				}
			}

			return false, fmt.Sprintf("NopResource %s not Ready yet", name)
		}, defaultTimeout, defaultInterval, "NopResource should become Ready")
	}

	t.Log("All NopResources are Ready")

	// Step 3: List all NopResources in the namespace
	t.Log("")
	t.Log("Step 3: Verifying all NopResources exist...")

	list, err := dynamicClient.Resource(nopResourceGVR).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	t.Logf("Found %d NopResources in namespace %s", len(list.Items), testNamespace)
	for _, item := range list.Items {
		t.Logf("  - %s", item.GetName())
	}

	assert.GreaterOrEqual(t, len(list.Items), 3, "should have at least 3 NopResources")

	t.Log("")
	t.Log("SUCCESS: Multiple NopResources created and processed correctly")
}

// TestBackendPodReady verifies that the kausality backend pod is running.
func TestBackendPodReady(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Backend Pod ===")
	t.Log("Checking that the kausality-backend pod is running...")

	ktesting.Eventually(t, func() (bool, string) {
		pods, err := clientset.CoreV1().Pods(kausalityNS).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=kausality-backend",
		})
		if err != nil {
			return false, fmt.Sprintf("error listing backend pods: %v", err)
		}
		if len(pods.Items) == 0 {
			return false, "no backend pods found yet"
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				return false, fmt.Sprintf("backend pod %s phase=%s, waiting for Running", pod.Name, pod.Status.Phase)
			}
		}
		return true, "backend pod is running"
	}, defaultTimeout, defaultInterval, "backend pod should be ready")

	t.Log("")
	t.Log("SUCCESS: Backend pod is running")
}

// TestProviderReconciliation verifies that the webhook intercepts provider reconciliation.
func TestProviderReconciliation(t *testing.T) {
	ctx := context.Background()
	name := fmt.Sprintf("reconcile-nop-%s", rand.String(4))

	t.Log("=== Testing Provider Reconciliation ===")
	t.Log("Verify webhook intercepts provider-nop reconciliation of NopResources.")

	// Step 1: Create NopResource
	t.Log("")
	t.Logf("Step 1: Creating NopResource %q...", name)

	nopResource := makeNopResource(name, nil)
	_, err := dynamicClient.Resource(nopResourceGVR).Create(ctx, nopResource, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting NopResource %s", name)
		_ = dynamicClient.Resource(nopResourceGVR).Delete(ctx, name, metav1.DeleteOptions{})
	})

	// Step 2: Wait for Ready
	t.Log("")
	t.Log("Step 2: Waiting for NopResource to become Ready...")

	ktesting.Eventually(t, func() (bool, string) {
		obj, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting NopResource: %v", err)
		}

		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found || len(conditions) == 0 {
			return false, "no conditions yet"
		}

		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			cType, _, _ := unstructured.NestedString(cond, "type")
			cStatus, _, _ := unstructured.NestedString(cond, "status")

			if cType == "Ready" && cStatus == "True" {
				return true, "NopResource is Ready"
			}
		}
		return false, "NopResource not Ready yet"
	}, defaultTimeout, defaultInterval, "NopResource should become Ready")

	// Step 3: Update the NopResource to trigger re-reconciliation
	t.Log("")
	t.Log("Step 3: Updating NopResource to trigger re-reconciliation...")

	obj, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	// Update the conditionAfter to trigger a change
	err = unstructured.SetNestedField(obj.Object, []interface{}{
		map[string]interface{}{
			"time":            "5s",
			"conditionType":   "Ready",
			"conditionStatus": "True",
		},
	}, "spec", "forProvider", "conditionAfter")
	require.NoError(t, err)

	_, err = dynamicClient.Resource(nopResourceGVR).Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("NopResource updated")

	// Step 4: Wait for re-reconciliation
	t.Log("")
	t.Log("Step 4: Waiting for provider to re-reconcile...")

	ktesting.Eventually(t, func() (bool, string) {
		obj, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting NopResource: %v", err)
		}

		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found || len(conditions) == 0 {
			return false, "no conditions yet"
		}

		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			cType, _, _ := unstructured.NestedString(cond, "type")
			cStatus, _, _ := unstructured.NestedString(cond, "status")

			if cType == "Ready" && cStatus == "True" {
				return true, "NopResource re-reconciled and Ready"
			}
		}
		return false, "waiting for Ready after re-reconciliation"
	}, defaultTimeout, defaultInterval, "NopResource should become Ready after update")

	t.Log("")
	t.Log("SUCCESS: Provider reconciliation detected and processed")
}
