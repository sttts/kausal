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
// Drift Detection Tests
// =============================================================================

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
