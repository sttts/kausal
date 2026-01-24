//go:build e2e

package kubernetes

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
)

// =============================================================================
// Admission Request Processing Tests
// =============================================================================

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
