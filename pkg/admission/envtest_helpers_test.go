//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kausality-io/kausality/pkg/controller"
)

// =============================================================================
// Helper Functions
// =============================================================================

var testCounter int

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstr(s, substr)))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func createDeployment(t *testing.T, ctx context.Context, namePrefix string) *appsv1.Deployment {
	t.Helper()
	testCounter++

	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNS,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, deploy); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	// Re-fetch to get server-set fields
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	return deploy
}

// markParentStable sets the phase annotation and status to make a parent appear stable (initialized).
// This simulates a parent that has completed initialization and is now in steady state.
func markParentStable(t *testing.T, ctx context.Context, deploy *appsv1.Deployment) {
	t.Helper()

	// Set phase annotation with retry
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return err
		}
		annotations := deploy.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
		deploy.SetAnnotations(annotations)
		return k8sClient.Update(ctx, deploy)
	})
	if err != nil {
		t.Fatalf("failed to update deployment annotations: %v", err)
	}

	// Set status with retry
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return err
		}
		deploy.Status.ObservedGeneration = deploy.Generation
		deploy.Status.Replicas = 1
		return k8sClient.Status().Update(ctx, deploy)
	})
	if err != nil {
		t.Fatalf("failed to update deployment status: %v", err)
	}

	// Re-fetch to get final state
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
}

func createReplicaSetWithOwner(t *testing.T, ctx context.Context, namePrefix string, owner *appsv1.Deployment) *appsv1.ReplicaSet {
	t.Helper()
	testCounter++

	trueVal := true
	replicas := int32(1)

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNS,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       owner.Name,
					UID:        owner.UID,
					Controller: &trueVal,
				},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	// Re-fetch
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get replicaset: %v", err)
	}

	return rs
}

func createDeploymentInNamespace(t *testing.T, ctx context.Context, namePrefix string, namespace string) *appsv1.Deployment {
	t.Helper()
	testCounter++

	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, deploy); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	return deploy
}

func createReplicaSetWithOwnerInNamespace(t *testing.T, ctx context.Context, namePrefix string, namespace string, owner *appsv1.Deployment) *appsv1.ReplicaSet {
	t.Helper()
	testCounter++

	trueVal := true
	replicas := int32(1)

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       owner.Name,
					UID:        owner.UID,
					Controller: &trueVal,
				},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get replicaset: %v", err)
	}

	return rs
}

func createDeploymentWithLabels(t *testing.T, ctx context.Context, namePrefix string, labels map[string]string) *appsv1.Deployment {
	t.Helper()
	testCounter++

	replicas := int32(1)

	// Merge with app label
	allLabels := map[string]string{"app": namePrefix}
	for k, v := range labels {
		allLabels[k] = v
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNS,
			Labels:    allLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, deploy); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	return deploy
}

func createReplicaSetWithOwnerAndLabels(t *testing.T, ctx context.Context, namePrefix string, owner *appsv1.Deployment, labels map[string]string) *appsv1.ReplicaSet {
	t.Helper()
	testCounter++

	trueVal := true
	replicas := int32(1)

	// Merge with app label
	allLabels := map[string]string{"app": namePrefix}
	for k, v := range labels {
		allLabels[k] = v
	}

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNS,
			Labels:    allLabels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       owner.Name,
					UID:        owner.UID,
					Controller: &trueVal,
				},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClient.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get replicaset: %v", err)
	}

	return rs
}

// =============================================================================
// Unit Test Helpers (use k8sClientUnit - no webhook)
// =============================================================================

func createDeploymentUnit(t *testing.T, ctx context.Context, namePrefix string) *appsv1.Deployment {
	t.Helper()
	testCounter++

	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNSUnit,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClientUnit.Create(ctx, deploy); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	return deploy
}

func markParentStableUnit(t *testing.T, ctx context.Context, deploy *appsv1.Deployment) {
	t.Helper()

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return err
		}
		annotations := deploy.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[controller.PhaseAnnotation] = controller.PhaseValueInitialized
		deploy.SetAnnotations(annotations)
		return k8sClientUnit.Update(ctx, deploy)
	})
	if err != nil {
		t.Fatalf("failed to update deployment annotations: %v", err)
	}

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return err
		}
		deploy.Status.ObservedGeneration = deploy.Generation
		deploy.Status.Replicas = 1
		return k8sClientUnit.Status().Update(ctx, deploy)
	})
	if err != nil {
		t.Fatalf("failed to update deployment status: %v", err)
	}

	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}
}

func createReplicaSetWithOwnerUnit(t *testing.T, ctx context.Context, namePrefix string, owner *appsv1.Deployment) *appsv1.ReplicaSet {
	t.Helper()
	testCounter++

	trueVal := true
	replicas := int32(1)

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNSUnit,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       owner.Name,
					UID:        owner.UID,
					Controller: &trueVal,
				},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClientUnit.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get replicaset: %v", err)
	}

	return rs
}

func createDeploymentInNamespaceUnit(t *testing.T, ctx context.Context, namePrefix string, namespace string) *appsv1.Deployment {
	t.Helper()
	testCounter++

	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClientUnit.Create(ctx, deploy); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	return deploy
}

func createReplicaSetWithOwnerInNamespaceUnit(t *testing.T, ctx context.Context, namePrefix string, namespace string, owner *appsv1.Deployment) *appsv1.ReplicaSet {
	t.Helper()
	testCounter++

	trueVal := true
	replicas := int32(1)

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       owner.Name,
					UID:        owner.UID,
					Controller: &trueVal,
				},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClientUnit.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get replicaset: %v", err)
	}

	return rs
}

func createDeploymentWithLabelsUnit(t *testing.T, ctx context.Context, namePrefix string, labels map[string]string) *appsv1.Deployment {
	t.Helper()
	testCounter++

	replicas := int32(1)

	allLabels := map[string]string{"app": namePrefix}
	for k, v := range labels {
		allLabels[k] = v
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNSUnit,
			Labels:    allLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClientUnit.Create(ctx, deploy); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
		t.Fatalf("failed to get deployment: %v", err)
	}

	return deploy
}

func createReplicaSetWithOwnerAndLabelsUnit(t *testing.T, ctx context.Context, namePrefix string, owner *appsv1.Deployment, labels map[string]string) *appsv1.ReplicaSet {
	t.Helper()
	testCounter++

	trueVal := true
	replicas := int32(1)

	allLabels := map[string]string{"app": namePrefix}
	for k, v := range labels {
		allLabels[k] = v
	}

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", namePrefix, testCounter),
			Namespace: testNSUnit,
			Labels:    allLabels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       owner.Name,
					UID:        owner.UID,
					Controller: &trueVal,
				},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": namePrefix},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": namePrefix},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
		},
	}

	if err := k8sClientUnit.Create(ctx, rs); err != nil {
		t.Fatalf("failed to create replicaset: %v", err)
	}

	if err := k8sClientUnit.Get(ctx, client.ObjectKeyFromObject(rs), rs); err != nil {
		t.Fatalf("failed to get replicaset: %v", err)
	}

	return rs
}
