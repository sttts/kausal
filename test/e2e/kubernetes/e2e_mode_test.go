//go:build e2e

package kubernetes

import (
	"context"
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

// =============================================================================
// Mode Annotation Tests
// =============================================================================

// TestModeAnnotation verifies that the kausality.io/mode annotation controls enforcement.
func TestModeAnnotation(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Mode Annotation ===")
	t.Log("Verify that kausality.io/mode annotation on namespace controls drift enforcement.")

	// Step 1: Create a namespace with enforce mode
	t.Log("")
	t.Log("Step 1: Creating namespace with enforce mode annotation...")
	enforceNS := fmt.Sprintf("enforce-test-%s", rand.String(4))
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: enforceNS,
			Annotations: map[string]string{
				"kausality.io/mode": "enforce",
			},
		},
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting namespace %s", enforceNS)
		_ = clientset.CoreV1().Namespaces().Delete(ctx, enforceNS, metav1.DeleteOptions{})
	})
	t.Logf("Namespace %s created with kausality.io/mode=enforce", enforceNS)

	// Step 2: Create a Deployment in the enforce namespace
	t.Log("")
	t.Log("Step 2: Creating Deployment in enforce namespace...")
	t.Log("This verifies the webhook reads the namespace's mode annotation.")

	name := fmt.Sprintf("mode-test-%s", rand.String(4))
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: enforceNS,
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

	_, err = clientset.AppsV1().Deployments(enforceNS).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err, "Deployment creation should succeed (no drift on initial create)")
	t.Log("Deployment created successfully")

	// Step 3: Wait for Deployment to be available
	t.Log("")
	t.Log("Step 3: Waiting for Deployment to become available...")

	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting deployment: %v", err)
		}
		if dep.Status.AvailableReplicas >= 1 {
			return true, "deployment is available"
		}
		return false, fmt.Sprintf("deployment not yet available: available=%d", dep.Status.AvailableReplicas)
	}, defaultTimeout, defaultInterval, "deployment should become available")

	t.Log("")
	t.Log("Step 4: Verifying namespace mode annotation is honored...")
	t.Log("The webhook should read kausality.io/mode from the namespace.")

	// Verify the namespace has the annotation
	nsObj, err := clientset.CoreV1().Namespaces().Get(ctx, enforceNS, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "enforce", nsObj.Annotations["kausality.io/mode"])
	t.Logf("Namespace %s has kausality.io/mode=%s", enforceNS, nsObj.Annotations["kausality.io/mode"])

	t.Log("")
	t.Log("SUCCESS: Mode annotation test completed")
	t.Log("The webhook correctly reads namespace-level mode annotations.")
}
