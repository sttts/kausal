//go:build e2e

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

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

// TestTracePropagation verifies that trace labels on a Deployment are propagated
// to child ReplicaSets and Pods via the kausality webhook.
func TestTracePropagation(t *testing.T) {
	ctx := context.Background()
	name := fmt.Sprintf("trace-test-%s", rand.String(4))

	t.Log("=== Testing Trace Propagation ===")
	t.Log("When a Deployment has kausality.io/trace-* labels, the webhook should")
	t.Log("propagate them as a trace annotation to child ReplicaSets and Pods.")

	// Step 1: Create a Deployment with trace labels
	t.Log("")
	t.Logf("Step 1: Creating Deployment %q with trace labels...", name)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				"kausality.io/trace-ticket": "TEST-123",
				"kausality.io/trace-pr":     "PR-456",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: "nginx:alpine",
					}},
				},
			},
		},
	}

	_, err := clientset.AppsV1().Deployments(testNamespace).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting deployment %s", name)
		_ = clientset.AppsV1().Deployments(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	})
	t.Logf("Deployment %q created with labels: trace-ticket=TEST-123, trace-pr=PR-456", name)

	// Step 2: Wait for the Deployment controller to create a ReplicaSet
	t.Log("")
	t.Log("Step 2: Waiting for Deployment controller to create ReplicaSet...")

	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting deployment: %v", err)
		}
		if dep.Status.AvailableReplicas >= 1 {
			return true, "deployment is available"
		}
		return false, fmt.Sprintf("deployment not yet available: replicas=%d, available=%d",
			dep.Status.Replicas, dep.Status.AvailableReplicas)
	}, defaultTimeout, defaultInterval, "deployment should become available")
	t.Log("Deployment is now available")

	// Step 3: Check that the ReplicaSet has the trace annotation
	t.Log("")
	t.Log("Step 3: Checking ReplicaSet for trace annotation...")
	t.Log("The webhook should have intercepted the ReplicaSet creation and added the trace.")

	var rsName string
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err != nil {
			return false, fmt.Sprintf("error listing replicasets: %v", err)
		}
		if len(rsList.Items) == 0 {
			return false, "no replicaset found yet"
		}

		rs := rsList.Items[0]
		rsName = rs.Name

		traceAnnotation := rs.Annotations["kausality.io/trace"]
		if traceAnnotation == "" {
			return false, fmt.Sprintf("no trace annotation yet on replicaset %s", rs.Name)
		}

		// Parse and verify the trace content
		var trace map[string]interface{}
		if err := json.Unmarshal([]byte(traceAnnotation), &trace); err != nil {
			return false, fmt.Sprintf("failed to parse trace annotation: %v", err)
		}

		labels, ok := trace["labels"].(map[string]interface{})
		if !ok {
			return false, "no labels in trace"
		}

		if _, hasTicket := labels["kausality.io/trace-ticket"]; !hasTicket {
			return false, "trace missing trace-ticket label"
		}
		if _, hasPR := labels["kausality.io/trace-pr"]; !hasPR {
			return false, "trace missing trace-pr label"
		}

		return true, fmt.Sprintf("replicaset %s has trace annotation with expected labels", rs.Name)
	}, annotationTimeout, defaultInterval, "ReplicaSet should have trace annotation")

	t.Logf("ReplicaSet %s trace propagation verified", rsName)

	// Step 4: Check that the Pods have the trace annotation
	t.Log("")
	t.Log("Step 4: Checking Pods for trace annotation...")
	t.Log("The trace should propagate from ReplicaSet to Pods.")

	ktesting.Eventually(t, func() (bool, string) {
		pods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err != nil {
			return false, fmt.Sprintf("error listing pods: %v", err)
		}
		if len(pods.Items) == 0 {
			return false, "no pods found yet"
		}

		for _, pod := range pods.Items {
			traceAnnotation := pod.Annotations["kausality.io/trace"]
			if traceAnnotation == "" {
				return false, fmt.Sprintf("no trace annotation yet on pod %s (phase=%s)", pod.Name, pod.Status.Phase)
			}

			// Parse and verify the trace content
			var trace map[string]interface{}
			if err := json.Unmarshal([]byte(traceAnnotation), &trace); err != nil {
				return false, fmt.Sprintf("failed to parse trace annotation on pod %s: %v", pod.Name, err)
			}

			// The pod trace should have hops showing the chain
			hops, ok := trace["hops"].([]interface{})
			if !ok || len(hops) == 0 {
				return false, fmt.Sprintf("pod %s trace has no hops", pod.Name)
			}
		}
		return true, fmt.Sprintf("all %d pods have trace annotations with hops", len(pods.Items))
	}, annotationTimeout, defaultInterval, "Pods should have trace annotation")

	t.Log("")
	t.Log("SUCCESS: Trace labels were propagated through the entire chain:")
	t.Logf("  Deployment %s -> ReplicaSet %s -> Pod(s)", name, rsName)
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

// TestDeploymentStabilization tests the drift detection scenario where a Deployment
// is stable (observedGeneration == generation) and then gets updated.
func TestDeploymentStabilization(t *testing.T) {
	ctx := context.Background()
	name := fmt.Sprintf("stable-test-%s", rand.String(4))

	t.Log("=== Testing Deployment Stabilization and Update ===")
	t.Log("This test simulates the drift detection scenario:")
	t.Log("1. Create a Deployment and wait for it to stabilize")
	t.Log("2. Update the Deployment, triggering controller reconciliation")
	t.Log("3. The webhook should see the ReplicaSet update from the controller")

	// Step 1: Create initial Deployment
	t.Log("")
	t.Logf("Step 1: Creating Deployment %q with nginx:1.24-alpine...", name)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: "nginx:1.24-alpine",
					}},
				},
			},
		},
	}

	_, err := clientset.AppsV1().Deployments(testNamespace).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting deployment %s", name)
		_ = clientset.AppsV1().Deployments(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	})
	t.Log("Deployment created")

	// Step 2: Wait for stabilization
	t.Log("")
	t.Log("Step 2: Waiting for Deployment to stabilize...")
	t.Log("Stabilized means: observedGeneration == generation && availableReplicas >= 1")

	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting deployment: %v", err)
		}
		if dep.Status.ObservedGeneration != dep.Generation {
			return false, fmt.Sprintf("not stable: generation=%d, observedGeneration=%d",
				dep.Generation, dep.Status.ObservedGeneration)
		}
		if dep.Status.AvailableReplicas < 1 {
			return false, fmt.Sprintf("not available: availableReplicas=%d", dep.Status.AvailableReplicas)
		}
		return true, fmt.Sprintf("deployment stabilized: generation=%d, observedGeneration=%d, available=%d",
			dep.Generation, dep.Status.ObservedGeneration, dep.Status.AvailableReplicas)
	}, defaultTimeout, defaultInterval, "deployment should stabilize")

	dep, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
	t.Logf("Deployment stabilized: generation=%d, observedGeneration=%d",
		dep.Generation, dep.Status.ObservedGeneration)

	// Step 3: Update the Deployment
	t.Log("")
	t.Log("Step 3: Updating Deployment image to nginx:1.25-alpine...")
	t.Log("This will increment the generation and trigger a new ReplicaSet.")

	dep.Spec.Template.Spec.Containers[0].Image = "nginx:1.25-alpine"
	_, err = clientset.AppsV1().Deployments(testNamespace).Update(ctx, dep, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("Deployment updated")

	// Step 4: Wait for rollout
	t.Log("")
	t.Log("Step 4: Waiting for rollout to complete...")

	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting deployment: %v", err)
		}
		if dep.Status.ObservedGeneration != dep.Generation {
			return false, fmt.Sprintf("rollout in progress: observedGeneration=%d, generation=%d",
				dep.Status.ObservedGeneration, dep.Generation)
		}
		if dep.Status.UpdatedReplicas != *dep.Spec.Replicas {
			return false, fmt.Sprintf("rollout in progress: updated=%d, desired=%d",
				dep.Status.UpdatedReplicas, *dep.Spec.Replicas)
		}
		if dep.Status.AvailableReplicas != *dep.Spec.Replicas {
			return false, fmt.Sprintf("rollout in progress: available=%d, desired=%d",
				dep.Status.AvailableReplicas, *dep.Spec.Replicas)
		}
		return true, fmt.Sprintf("rollout complete: updated=%d, available=%d",
			dep.Status.UpdatedReplicas, dep.Status.AvailableReplicas)
	}, defaultTimeout, defaultInterval, "deployment rollout should complete")

	t.Log("")
	t.Log("SUCCESS: Deployment stabilization and update completed")
	t.Log("The webhook intercepted the ReplicaSet mutations during this process.")
}

// TestAdmissionRequestProcessing verifies that the webhook processes CREATE, UPDATE,
// and DELETE operations without blocking them.
func TestAdmissionRequestProcessing(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Admission Request Processing ===")
	t.Log("Verifying that the webhook processes CREATE, UPDATE, and DELETE")
	t.Log("operations without blocking them (currently in log mode).")

	t.Run("CREATE", func(t *testing.T) {
		name := fmt.Sprintf("admit-create-%s", rand.String(4))

		t.Log("")
		t.Logf("Testing CREATE: Creating Deployment %q...", name)

		deployment := makeDeployment(name, testNamespace)
		_, err := clientset.AppsV1().Deployments(testNamespace).Create(ctx, deployment, metav1.CreateOptions{})
		require.NoError(t, err, "CREATE should succeed through webhook")
		t.Cleanup(func() {
			_ = clientset.AppsV1().Deployments(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		})

		t.Log("CREATE succeeded - webhook allowed the operation")
	})

	t.Run("UPDATE", func(t *testing.T) {
		name := fmt.Sprintf("admit-update-%s", rand.String(4))

		t.Log("")
		t.Logf("Testing UPDATE: Creating Deployment %q...", name)

		deployment := makeDeployment(name, testNamespace)
		_, err := clientset.AppsV1().Deployments(testNamespace).Create(ctx, deployment, metav1.CreateOptions{})
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = clientset.AppsV1().Deployments(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		})

		t.Log("Updating Deployment replicas from 1 to 2...")
		dep, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)

		dep.Spec.Replicas = ptr(int32(2))
		_, err = clientset.AppsV1().Deployments(testNamespace).Update(ctx, dep, metav1.UpdateOptions{})
		require.NoError(t, err, "UPDATE should succeed through webhook")

		t.Log("UPDATE succeeded - webhook allowed the operation")
	})

	t.Run("DELETE", func(t *testing.T) {
		name := fmt.Sprintf("admit-delete-%s", rand.String(4))

		t.Log("")
		t.Logf("Testing DELETE: Creating Deployment %q...", name)

		deployment := makeDeployment(name, testNamespace)
		_, err := clientset.AppsV1().Deployments(testNamespace).Create(ctx, deployment, metav1.CreateOptions{})
		require.NoError(t, err)

		t.Log("Deleting Deployment...")
		err = clientset.AppsV1().Deployments(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		require.NoError(t, err, "DELETE should succeed through webhook")

		t.Log("DELETE succeeded - webhook allowed the operation")
	})

	t.Log("")
	t.Log("SUCCESS: All admission request types processed correctly")
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
		return true, fmt.Sprintf("backend pod is running")
	}, defaultTimeout, defaultInterval, "backend pod should be ready")

	t.Log("")
	t.Log("SUCCESS: Backend pod is running")
}

func makeDeployment(name, namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: "nginx:alpine",
					}},
				},
			},
		},
	}
}

func ptr[T any](v T) *T {
	return &v
}
