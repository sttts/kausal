//go:build e2e

package kubernetes

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

// =============================================================================
// Backend Tests
// =============================================================================

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

// TestBackendReceivesDriftReports verifies that DriftReports are sent to the backend
// when drift is detected. This test triggers a drift scenario and checks the backend logs.
func TestBackendReceivesDriftReports(t *testing.T) {
	ctx := context.Background()
	name := fmt.Sprintf("drift-backend-%s", rand.String(4))

	t.Log("=== Testing Backend DriftReport Reception ===")
	t.Log("When drift is detected, the webhook should send a DriftReport to the backend.")

	// Step 1: Create a Deployment and wait for it to stabilize
	t.Log("")
	t.Logf("Step 1: Creating Deployment %q and waiting for stabilization...", name)
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

	// Wait for stabilization
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting deployment: %v", err)
		}
		if dep.Status.ObservedGeneration != dep.Generation {
			return false, fmt.Sprintf("not stable: gen=%d, obsGen=%d", dep.Generation, dep.Status.ObservedGeneration)
		}
		if dep.Status.AvailableReplicas < 1 {
			return false, fmt.Sprintf("not available: replicas=%d", dep.Status.AvailableReplicas)
		}
		return true, "deployment stabilized"
	}, defaultTimeout, defaultInterval, "deployment should stabilize")
	t.Log("Deployment stabilized")

	// Step 2: Update the deployment to trigger drift
	t.Log("")
	t.Log("Step 2: Updating Deployment to trigger drift...")
	dep, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	dep.Spec.Template.Spec.Containers[0].Image = "nginx:1.25-alpine"
	_, err = clientset.AppsV1().Deployments(testNamespace).Update(ctx, dep, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("Deployment updated - this should trigger drift detection on the new ReplicaSet")

	// Wait for rollout to complete
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting deployment: %v", err)
		}
		if dep.Status.ObservedGeneration != dep.Generation {
			return false, "rollout in progress"
		}
		if dep.Status.AvailableReplicas != *dep.Spec.Replicas {
			return false, fmt.Sprintf("not available: available=%d, desired=%d", dep.Status.AvailableReplicas, *dep.Spec.Replicas)
		}
		return true, "rollout complete"
	}, defaultTimeout, defaultInterval, "rollout should complete")

	// Step 3: Check backend logs for DriftReport
	t.Log("")
	t.Log("Step 3: Checking backend logs for DriftReport...")

	ktesting.Eventually(t, func() (bool, string) {
		pods, err := clientset.CoreV1().Pods(kausalityNS).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=kausality-backend",
		})
		if err != nil {
			return false, fmt.Sprintf("error listing backend pods: %v", err)
		}
		if len(pods.Items) == 0 {
			return false, "no backend pods found"
		}

		// Get logs from the backend pod
		podName := pods.Items[0].Name
		req := clientset.CoreV1().Pods(kausalityNS).GetLogs(podName, &corev1.PodLogOptions{
			TailLines: ptr(int64(1000)),
		})
		logs, err := req.Do(ctx).Raw()
		if err != nil {
			return false, fmt.Sprintf("error getting logs: %v", err)
		}

		logStr := string(logs)

		// Check for DriftReport markers in the logs
		if !contains(logStr, "apiVersion: kausality.io") && !contains(logStr, "kind: DriftReport") {
			return false, "no DriftReport found in backend logs yet"
		}

		// Check for the specific deployment name or phase
		if !contains(logStr, "phase: Detected") && !contains(logStr, "phase: Resolved") {
			return false, "DriftReport found but no phase detected"
		}

		return true, "DriftReport found in backend logs"
	}, annotationTimeout, defaultInterval, "backend should receive DriftReport")

	t.Log("")
	t.Log("SUCCESS: Backend received DriftReport from webhook")
}
