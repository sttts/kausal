//go:build e2e

package kubernetes

import (
	"context"
	"encoding/json"
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
// Trace Propagation Tests
// =============================================================================

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

		// Parse the trace as an array of hops
		var hops []map[string]interface{}
		if err := json.Unmarshal([]byte(traceAnnotation), &hops); err != nil {
			return false, fmt.Sprintf("failed to parse trace annotation: %v", err)
		}

		if len(hops) == 0 {
			return false, "trace has no hops"
		}

		// Check that trace labels propagated (in origin hop or any hop)
		foundTicket := false
		foundPR := false
		for _, hop := range hops {
			labels, ok := hop["labels"].(map[string]interface{})
			if ok {
				if _, hasTicket := labels["ticket"]; hasTicket {
					foundTicket = true
				}
				if _, hasPR := labels["pr"]; hasPR {
					foundPR = true
				}
			}
		}

		if !foundTicket {
			return false, "trace missing ticket label"
		}
		if !foundPR {
			return false, "trace missing pr label"
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

			// Parse the trace as an array of hops
			var hops []map[string]interface{}
			if err := json.Unmarshal([]byte(traceAnnotation), &hops); err != nil {
				return false, fmt.Sprintf("failed to parse trace annotation on pod %s: %v", pod.Name, err)
			}

			// The pod trace should have hops showing the chain (Deployment -> ReplicaSet -> Pod)
			if len(hops) < 2 {
				return false, fmt.Sprintf("pod %s trace has only %d hops (expected >=2)", pod.Name, len(hops))
			}
		}
		return true, fmt.Sprintf("all %d pods have trace annotations with hops", len(pods.Items))
	}, annotationTimeout, defaultInterval, "Pods should have trace annotation")

	t.Log("")
	t.Log("SUCCESS: Trace labels were propagated through the entire chain:")
	t.Logf("  Deployment %s -> ReplicaSet %s -> Pod(s)", name, rsName)
}
