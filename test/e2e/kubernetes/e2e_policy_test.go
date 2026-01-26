//go:build e2e

package kubernetes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

// =============================================================================
// Kausality CRD Policy Tests
//
// These tests verify that drift detection mode is correctly resolved from
// Kausality CRD policies and that policy changes take effect dynamically.
// =============================================================================

// TestPolicyEnforceModeBlocksDrift verifies that a Kausality policy with enforce mode
// blocks drift for resources it matches.
func TestPolicyEnforceModeBlocksDrift(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Kausality Policy Enforce Mode ===")
	t.Log("When a Kausality policy specifies enforce mode for a namespace,")
	t.Log("drift should be blocked without needing a namespace annotation.")

	// Step 1: Create a namespace for this test (no mode annotation)
	policyNS := fmt.Sprintf("policy-enforce-%s", rand.String(4))
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyNS,
			// No kausality.io/mode annotation - mode comes from CRD
		},
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting namespace %s", policyNS)
		_ = clientset.CoreV1().Namespaces().Delete(ctx, policyNS, metav1.DeleteOptions{})
	})
	t.Logf("Created namespace %s (no mode annotation)", policyNS)

	// Step 2: Create a Kausality policy with enforce mode for this namespace
	policyName := fmt.Sprintf("policy-enforce-%s", rand.String(4))
	policy := &kausalityv1alpha1.Kausality{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: kausalityv1alpha1.KausalitySpec{
			Resources: []kausalityv1alpha1.ResourceRule{{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "replicasets"},
			}},
			Namespaces: &kausalityv1alpha1.NamespaceSelector{
				Names: []string{policyNS},
			},
			Mode: kausalityv1alpha1.ModeEnforce,
		},
	}
	err = kausalityClient.Create(ctx, policy)
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting Kausality policy %s", policyName)
		_ = kausalityClient.Delete(ctx, policy)
	})
	t.Logf("Created Kausality policy %s with enforce mode for namespace %s", policyName, policyNS)

	// Step 3: Wait for policy to be ready and webhook to be configured
	t.Log("")
	t.Log("Step 3: Waiting for policy to be ready...")
	ktesting.Eventually(t, func() (bool, string) {
		var p kausalityv1alpha1.Kausality
		if err := kausalityClient.Get(ctx, client.ObjectKey{Name: policyName}, &p); err != nil {
			return false, fmt.Sprintf("error getting policy: %v", err)
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
				return true, "policy is ready"
			}
		}
		return false, "waiting for Ready condition"
	}, defaultTimeout, defaultInterval, "policy should become ready")

	// Step 4: Create a Deployment
	t.Log("")
	t.Log("Step 4: Creating Deployment in policy namespace...")
	name := fmt.Sprintf("policy-test-%s", rand.String(4))
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: policyNS,
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

	_, err = clientset.AppsV1().Deployments(policyNS).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Logf("Created Deployment %s", name)

	// Step 5: Wait for stabilization
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(policyNS).Get(ctx, name, metav1.GetOptions{})
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

	// Step 6: Get the ReplicaSet
	var rsName string
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(policyNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err != nil || len(rsList.Items) == 0 {
			return false, "no replicaset found"
		}
		rsName = rsList.Items[0].Name
		return true, fmt.Sprintf("found replicaset %s", rsName)
	}, defaultTimeout, defaultInterval, "replicaset should exist")

	// Step 7: Directly modify the ReplicaSet (trigger drift)
	t.Log("")
	t.Log("Step 7: Modifying ReplicaSet to trigger drift...")
	rs, err := clientset.AppsV1().ReplicaSets(policyNS).Get(ctx, rsName, metav1.GetOptions{})
	require.NoError(t, err)
	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(policyNS).Update(ctx, rs, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("Modified ReplicaSet spec.replicas to 2")

	// Step 8: Verify drift is blocked (replicas should stay at 2)
	t.Log("")
	t.Log("Step 8: Verifying drift is blocked by policy...")
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(policyNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting replicaset: %v", err)
		}
		if *rs.Spec.Replicas != 2 {
			return false, fmt.Sprintf("replicas changed to %d (drift allowed!)", *rs.Spec.Replicas)
		}
		return true, "replicas still 2 (drift blocked by policy)"
	}, defaultTimeout, defaultInterval, "policy should block drift")

	t.Log("")
	t.Log("SUCCESS: Kausality policy enforce mode blocks drift")
	t.Log("Mode was determined from CRD, not namespace annotation")
}

// TestPolicyModeUpdate verifies that updating a Kausality policy's mode
// takes effect immediately.
func TestPolicyModeUpdate(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Kausality Policy Mode Update ===")
	t.Log("When a policy is updated from enforce to log mode,")
	t.Log("drift should become allowed without restarting the webhook.")

	// Step 1: Create a namespace for this test
	updateNS := fmt.Sprintf("policy-update-%s", rand.String(4))
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: updateNS,
		},
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting namespace %s", updateNS)
		_ = clientset.CoreV1().Namespaces().Delete(ctx, updateNS, metav1.DeleteOptions{})
	})
	t.Logf("Created namespace %s", updateNS)

	// Step 2: Create a policy with enforce mode
	policyName := fmt.Sprintf("policy-update-%s", rand.String(4))
	policy := &kausalityv1alpha1.Kausality{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: kausalityv1alpha1.KausalitySpec{
			Resources: []kausalityv1alpha1.ResourceRule{{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "replicasets"},
			}},
			Namespaces: &kausalityv1alpha1.NamespaceSelector{
				Names: []string{updateNS},
			},
			Mode: kausalityv1alpha1.ModeEnforce,
		},
	}
	err = kausalityClient.Create(ctx, policy)
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting Kausality policy %s", policyName)
		_ = kausalityClient.Delete(ctx, policy)
	})
	t.Logf("Created policy %s with enforce mode", policyName)

	// Wait for policy to be ready
	ktesting.Eventually(t, func() (bool, string) {
		var p kausalityv1alpha1.Kausality
		if err := kausalityClient.Get(ctx, client.ObjectKey{Name: policyName}, &p); err != nil {
			return false, fmt.Sprintf("error getting policy: %v", err)
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
				return true, "policy is ready"
			}
		}
		return false, "waiting for Ready condition"
	}, defaultTimeout, defaultInterval, "policy should become ready")

	// Step 3: Create a Deployment
	t.Log("")
	t.Log("Step 3: Creating Deployment...")
	name := fmt.Sprintf("update-test-%s", rand.String(4))
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: updateNS,
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

	_, err = clientset.AppsV1().Deployments(updateNS).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for stabilization
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(updateNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		if dep.Status.ObservedGeneration != dep.Generation || dep.Status.AvailableReplicas < 1 {
			return false, "not stable"
		}
		return true, "deployment stabilized"
	}, defaultTimeout, defaultInterval, "deployment should stabilize")
	t.Log("Deployment stabilized")

	// Get ReplicaSet
	var rsName string
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(updateNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err != nil || len(rsList.Items) == 0 {
			return false, "no replicaset found"
		}
		rsName = rsList.Items[0].Name
		return true, "found replicaset"
	}, defaultTimeout, defaultInterval, "replicaset should exist")

	// Step 4: Verify enforce mode blocks drift
	t.Log("")
	t.Log("Step 4: Verifying enforce mode blocks drift...")

	// First verify the webhook configuration is updated (status/ready fields check already does this)
	// Add extra wait for webhook to pick up the policy
	time.Sleep(2 * time.Second)

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		rs, err := clientset.AppsV1().ReplicaSets(updateNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		rs.Spec.Replicas = ptr(int32(2))
		_, err = clientset.AppsV1().ReplicaSets(updateNS).Update(ctx, rs, metav1.UpdateOptions{})
		return err
	})
	require.NoError(t, err)
	t.Log("Modified ReplicaSet replicas to 2")

	// Wait and verify replicas stay at 2 (drift is blocked)
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(updateNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		if *rs.Spec.Replicas != 2 {
			return false, fmt.Sprintf("replicas=%d (drift was allowed!)", *rs.Spec.Replicas)
		}
		return true, "replicas=2 (drift blocked)"
	}, defaultTimeout, defaultInterval, "drift should be blocked in enforce mode")
	t.Log("Drift blocked as expected (enforce mode)")

	// Reset RS to 1 for next test
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		rs, err := clientset.AppsV1().ReplicaSets(updateNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		rs.Spec.Replicas = ptr(int32(1))
		_, err = clientset.AppsV1().ReplicaSets(updateNS).Update(ctx, rs, metav1.UpdateOptions{})
		return err
	})
	require.NoError(t, err)

	// Step 5: Update policy to log mode
	t.Log("")
	t.Log("Step 5: Updating policy to log mode...")
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var p kausalityv1alpha1.Kausality
		if err := kausalityClient.Get(ctx, client.ObjectKey{Name: policyName}, &p); err != nil {
			return err
		}
		p.Spec.Mode = kausalityv1alpha1.ModeLog
		return kausalityClient.Update(ctx, &p)
	})
	require.NoError(t, err)
	t.Log("Policy updated to log mode")

	// Wait for policy store to refresh (the webhook should pick up the new mode)
	time.Sleep(2 * time.Second)

	// Step 6: Verify log mode allows drift
	t.Log("")
	t.Log("Step 6: Verifying log mode allows drift...")
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		rs, err := clientset.AppsV1().ReplicaSets(updateNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		rs.Spec.Replicas = ptr(int32(2))
		_, err = clientset.AppsV1().ReplicaSets(updateNS).Update(ctx, rs, metav1.UpdateOptions{})
		return err
	})
	require.NoError(t, err)
	t.Log("Modified ReplicaSet replicas to 2 again")

	// In log mode, controller should be able to fix it back to 1
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(updateNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		if *rs.Spec.Replicas != 1 {
			return false, fmt.Sprintf("replicas=%d, waiting for controller to fix", *rs.Spec.Replicas)
		}
		return true, "controller fixed replicas (drift allowed in log mode)"
	}, defaultTimeout, defaultInterval, "drift should be allowed in log mode")

	t.Log("")
	t.Log("SUCCESS: Policy mode update takes effect immediately")
	t.Log("- Enforce mode blocked drift")
	t.Log("- After updating to log mode, drift was allowed")
}

// TestPolicyOverrides verifies that policy overrides work correctly.
func TestPolicyOverrides(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Kausality Policy Overrides ===")
	t.Log("A policy can have mode=log by default but enforce for specific namespaces.")

	// Step 1: Create two namespaces - one for log mode, one for enforce
	logNS := fmt.Sprintf("override-log-%s", rand.String(4))
	enforceNS := fmt.Sprintf("override-enforce-%s", rand.String(4))

	for _, nsName := range []string{logNS, enforceNS} {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: nsName},
		}
		_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		require.NoError(t, err)
		t.Cleanup(func() {
			t.Logf("Cleanup: Deleting namespace %s", nsName)
			_ = clientset.CoreV1().Namespaces().Delete(ctx, nsName, metav1.DeleteOptions{})
		})
	}
	t.Logf("Created namespaces: %s (log), %s (enforce via override)", logNS, enforceNS)

	// Step 2: Create policy with log mode default and enforce override for one namespace
	policyName := fmt.Sprintf("policy-override-%s", rand.String(4))
	policy := &kausalityv1alpha1.Kausality{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: kausalityv1alpha1.KausalitySpec{
			Resources: []kausalityv1alpha1.ResourceRule{{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "replicasets"},
			}},
			Namespaces: &kausalityv1alpha1.NamespaceSelector{
				Names: []string{logNS, enforceNS},
			},
			Mode: kausalityv1alpha1.ModeLog, // Default
			Overrides: []kausalityv1alpha1.ModeOverride{{
				Namespaces: []string{enforceNS},
				Mode:       kausalityv1alpha1.ModeEnforce,
			}},
		},
	}
	err := kausalityClient.Create(ctx, policy)
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting Kausality policy %s", policyName)
		_ = kausalityClient.Delete(ctx, policy)
	})
	t.Logf("Created policy %s with log default and enforce override for %s", policyName, enforceNS)

	// Wait for policy to be ready
	ktesting.Eventually(t, func() (bool, string) {
		var p kausalityv1alpha1.Kausality
		if err := kausalityClient.Get(ctx, client.ObjectKey{Name: policyName}, &p); err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
				return true, "policy ready"
			}
		}
		return false, "waiting for Ready"
	}, defaultTimeout, defaultInterval, "policy should be ready")

	// Step 3: Create deployments in both namespaces
	t.Log("")
	t.Log("Step 3: Creating deployments in both namespaces...")

	createDeploymentAndWaitStable := func(namespace string) string {
		name := fmt.Sprintf("override-%s", rand.String(4))
		deployment := &appsv1.Deployment{
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
							Image: "nginx:1.24-alpine",
						}},
					},
				},
			},
		}
		_, err := clientset.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
		require.NoError(t, err)

		ktesting.Eventually(t, func() (bool, string) {
			dep, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Sprintf("error: %v", err)
			}
			if dep.Status.ObservedGeneration != dep.Generation || dep.Status.AvailableReplicas < 1 {
				return false, "not stable"
			}
			return true, "stable"
		}, defaultTimeout, defaultInterval, "deployment should stabilize")

		return name
	}

	logDepName := createDeploymentAndWaitStable(logNS)
	enforceDepName := createDeploymentAndWaitStable(enforceNS)
	t.Logf("Created deployments: %s/%s, %s/%s", logNS, logDepName, enforceNS, enforceDepName)

	// Get ReplicaSets
	getRSName := func(namespace, depName string) string {
		var rsName string
		ktesting.Eventually(t, func() (bool, string) {
			rsList, err := clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app=%s", depName),
			})
			if err != nil || len(rsList.Items) == 0 {
				return false, "no replicaset"
			}
			rsName = rsList.Items[0].Name
			return true, "found"
		}, defaultTimeout, defaultInterval, "rs should exist")
		return rsName
	}

	logRSName := getRSName(logNS, logDepName)
	enforceRSName := getRSName(enforceNS, enforceDepName)

	// Step 4: Test log namespace (drift should be allowed)
	t.Log("")
	t.Log("Step 4: Testing log namespace (drift should be allowed)...")

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		rs, err := clientset.AppsV1().ReplicaSets(logNS).Get(ctx, logRSName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		rs.Spec.Replicas = ptr(int32(2))
		_, err = clientset.AppsV1().ReplicaSets(logNS).Update(ctx, rs, metav1.UpdateOptions{})
		return err
	})
	require.NoError(t, err)

	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(logNS).Get(ctx, logRSName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		if *rs.Spec.Replicas != 1 {
			return false, fmt.Sprintf("replicas=%d, waiting for fix", *rs.Spec.Replicas)
		}
		return true, "controller fixed (drift allowed in log)"
	}, defaultTimeout, defaultInterval, "drift allowed in log namespace")
	t.Logf("LOG namespace %s: drift was allowed", logNS)

	// Step 5: Test enforce namespace (drift should be blocked)
	t.Log("")
	t.Log("Step 5: Testing enforce namespace (drift should be blocked)...")

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, enforceRSName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		rs.Spec.Replicas = ptr(int32(2))
		_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{})
		return err
	})
	require.NoError(t, err)

	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, enforceRSName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		if *rs.Spec.Replicas != 2 {
			return false, fmt.Sprintf("replicas=%d (drift allowed!)", *rs.Spec.Replicas)
		}
		return true, "replicas=2 (drift blocked by override)"
	}, defaultTimeout, defaultInterval, "drift blocked in enforce namespace")
	t.Logf("ENFORCE namespace %s: drift was blocked via override", enforceNS)

	t.Log("")
	t.Log("SUCCESS: Policy overrides work correctly")
	t.Log("- Log mode namespace: drift allowed (default)")
	t.Log("- Enforce mode namespace: drift blocked (via override)")
}
