//go:build e2e

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"

	"github.com/kausality-io/kausality/pkg/approval"
	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

// =============================================================================
// Drift and Approval Tests
//
// Drift = controller modifying a child's spec when the parent is stable (gen == obsGen).
// We trigger drift by directly editing the ReplicaSet's spec.replicas, causing
// the Deployment controller to try to "fix" it back to the desired count.
//
// Note: Kausality only intercepts spec changes - metadata/status changes are ignored.
// =============================================================================

// TestDriftBlockedInEnforceMode verifies that drift is blocked in enforce mode
// when there is no approval annotation.
func TestDriftBlockedInEnforceMode(t *testing.T) {
	if clientset == nil {
		t.Fatal("clientset is nil - TestMain did not initialize properly")
	}
	ctx := context.Background()

	t.Log("=== Testing Drift Blocked in Enforce Mode ===")
	t.Log("When we directly modify a ReplicaSet's spec and the Deployment controller tries to fix it,")
	t.Log("that fix attempt is drift (controller updating when parent is stable).")
	t.Log("In enforce mode without approval, drift should be blocked.")

	// Step 1: Create a namespace with enforce mode
	enforceNS := fmt.Sprintf("drift-block-%s", rand.String(4))
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
	t.Logf("Created namespace %s with enforce mode", enforceNS)

	// Step 2: Create a Deployment with 1 replica
	name := fmt.Sprintf("drift-block-%s", rand.String(4))
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
						Image: "nginx:1.24-alpine",
					}},
				},
			},
		},
	}

	_, err = clientset.AppsV1().Deployments(enforceNS).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Logf("Created Deployment %s with 1 replica", name)

	// Step 3: Wait for stabilization (gen == obsGen)
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
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
	t.Log("Deployment stabilized (gen == obsGen)")

	// Step 4: Get the ReplicaSet
	var rs *appsv1.ReplicaSet
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(enforceNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err != nil {
			return false, fmt.Sprintf("error listing replicasets: %v", err)
		}
		if len(rsList.Items) == 0 {
			return false, "no replicaset found"
		}
		rs = &rsList.Items[0]
		return true, fmt.Sprintf("found replicaset %s with %d replicas", rs.Name, *rs.Spec.Replicas)
	}, defaultTimeout, defaultInterval, "replicaset should exist")

	// Step 5: Directly modify the ReplicaSet's spec.replicas (simulate external drift)
	// Change from 1 to 2 - the Deployment controller will want to set it back to 1
	// Use a specific fieldManager so kausality knows this isn't the controller
	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{
		FieldManager: "e2e-test",
	})
	require.NoError(t, err)
	t.Log("Directly modified ReplicaSet spec.replicas from 1 to 2")

	// Step 6: Wait for controller to attempt reconciliation
	// The controller will try to set replicas back to 1, but that's drift and should be blocked
	t.Log("Waiting for controller to attempt reconciliation (which should be blocked)...")

	// Step 7: Verify our modification persists (controller couldn't fix it)
	// The ReplicaSet should still have 2 replicas because the controller's fix was blocked
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rs.Name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting replicaset: %v", err)
		}
		if *rs.Spec.Replicas != 2 {
			return false, fmt.Sprintf("replicas changed to %d (drift was allowed!)", *rs.Spec.Replicas)
		}
		return true, "replicas still 2 (drift blocked)"
	}, defaultTimeout, defaultInterval, "drift should be blocked")

	t.Log("")
	t.Log("SUCCESS: Drift was blocked - our modification to the ReplicaSet persisted")
}

// TestApprovalAllowsDrift verifies that kausality.io/approvals annotation allows
// drift to pass in enforce mode.
func TestApprovalAllowsDrift(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Approval Allows Drift ===")
	t.Log("When we directly modify a ReplicaSet's spec and the Deployment controller tries to fix it,")
	t.Log("with an approval annotation on the Deployment, the drift should be allowed.")

	// Step 1: Create a namespace with enforce mode
	enforceNS := fmt.Sprintf("approve-drift-%s", rand.String(4))
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
	t.Logf("Created namespace %s with enforce mode", enforceNS)

	// Step 2: Create a Deployment with approval for all ReplicaSets
	name := fmt.Sprintf("approve-drift-%s", rand.String(4))

	approvals := []approval.Approval{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "*", // Approve all ReplicaSets
		Mode:       approval.ModeAlways,
	}}
	approvalData, err := json.Marshal(approvals)
	require.NoError(t, err)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: enforceNS,
			Annotations: map[string]string{
				approval.ApprovalsAnnotation: string(approvalData),
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
						Image: "nginx:1.24-alpine",
					}},
				},
			},
		},
	}

	_, err = clientset.AppsV1().Deployments(enforceNS).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Logf("Created Deployment %s with approval annotation", name)

	// Step 3: Wait for stabilization (gen == obsGen)
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
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
	t.Log("Deployment stabilized (gen == obsGen)")

	// Step 4: Get the ReplicaSet
	var rsName string
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(enforceNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err != nil {
			return false, fmt.Sprintf("error listing replicasets: %v", err)
		}
		if len(rsList.Items) == 0 {
			return false, "no replicaset found"
		}
		rsName = rsList.Items[0].Name
		return true, fmt.Sprintf("found replicaset %s", rsName)
	}, defaultTimeout, defaultInterval, "replicaset should exist")

	// Step 5: Directly modify the ReplicaSet's spec.replicas (simulate external drift)
	rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
	require.NoError(t, err)

	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("Directly modified ReplicaSet spec.replicas from 1 to 2")

	// Step 6: Wait for controller to fix the drift (should be allowed with approval)
	t.Log("Waiting for controller to reconcile (drift should be allowed with approval)...")

	// Step 7: Verify the controller was able to fix it (replicas should be back to 1)
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting replicaset: %v", err)
		}
		if *rs.Spec.Replicas != 1 {
			return false, fmt.Sprintf("replicas still %d (controller hasn't reconciled yet)", *rs.Spec.Replicas)
		}
		return true, "replicas back to 1 (controller reconciled successfully)"
	}, defaultTimeout, defaultInterval, "controller should fix the drift")

	t.Log("")
	t.Log("SUCCESS: Drift was allowed - controller successfully fixed the ReplicaSet")
}

// TestRejectionOverridesApproval verifies that kausality.io/rejections annotation blocks
// controller drift EVEN when there is an approval for the same child.
// This proves rejection takes precedence over approval.
//
// Important: Rejection only blocks CONTROLLER DRIFT (controller updating when parent is stable).
// User modifications are NOT drift - they create a new causal origin.
func TestRejectionOverridesApproval(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Rejection Overrides Approval ===")
	t.Log("When a parent has BOTH approval AND rejection for a child,")
	t.Log("rejection takes precedence and controller drift should be blocked.")
	t.Log("")
	t.Log("This test proves rejection makes a difference by showing:")
	t.Log("1. With approval alone, controller drift IS allowed (counter-example)")
	t.Log("2. With approval + rejection, controller drift is BLOCKED (rejection wins)")
	t.Log("")
	t.Log("Note: User modifications are NOT drift - they create a new causal origin.")

	// Step 1: Create a namespace with enforce mode
	enforceNS := fmt.Sprintf("reject-override-%s", rand.String(4))
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
	t.Logf("Created namespace %s with enforce mode", enforceNS)

	// Step 2: Create a Deployment WITH approval for all ReplicaSets
	t.Log("")
	t.Log("Step 2: Creating Deployment WITH approval annotation...")
	name := fmt.Sprintf("reject-override-%s", rand.String(4))

	approvals := []approval.Approval{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "*", // Approve all ReplicaSets
		Mode:       approval.ModeAlways,
	}}
	approvalData, err := json.Marshal(approvals)
	require.NoError(t, err)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: enforceNS,
			Annotations: map[string]string{
				approval.ApprovalsAnnotation: string(approvalData),
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
						Image: "nginx:1.24-alpine",
					}},
				},
			},
		},
	}

	_, err = clientset.AppsV1().Deployments(enforceNS).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Logf("Created Deployment %s with approval annotation", name)

	// Step 3: Wait for stabilization
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
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

	// Step 4: Get the ReplicaSet name
	var rsName string
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(enforceNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err != nil {
			return false, fmt.Sprintf("error listing replicasets: %v", err)
		}
		if len(rsList.Items) == 0 {
			return false, "no replicaset found"
		}
		rsName = rsList.Items[0].Name
		return true, fmt.Sprintf("found replicaset %s", rsName)
	}, defaultTimeout, defaultInterval, "replicaset should exist")

	// Step 5: COUNTER-EXAMPLE - Verify approval allows controller drift
	t.Log("")
	t.Log("Step 5: COUNTER-EXAMPLE - Verify approval allows controller drift...")
	t.Log("User modifies ReplicaSet replicas from 1 to 2 (this is NOT drift)...")

	rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
	require.NoError(t, err)
	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("User modified ReplicaSet spec.replicas to 2 (new causal origin)")

	// Wait for controller to fix it back (this IS drift, should be allowed with approval)
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting replicaset: %v", err)
		}
		if *rs.Spec.Replicas != 1 {
			return false, fmt.Sprintf("replicas still %d, waiting for controller to fix it", *rs.Spec.Replicas)
		}
		return true, "controller fixed replicas back to 1 (drift allowed with approval)"
	}, defaultTimeout, defaultInterval, "approval should allow controller drift")
	t.Log("COUNTER-EXAMPLE PASSED: With approval alone, controller drift was allowed")

	// Step 6: Add rejection annotation (keeping approval)
	t.Log("")
	t.Log("Step 6: Adding rejection annotation (keeping approval)...")
	dep, err := clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	rejections := []approval.Rejection{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       rsName,
		Reason:     "frozen by test",
	}}
	rejectionData, err := json.Marshal(rejections)
	require.NoError(t, err)

	dep.Annotations[approval.RejectionsAnnotation] = string(rejectionData)
	_, err = clientset.AppsV1().Deployments(enforceNS).Update(ctx, dep, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Logf("Added rejection annotation (approval still present)")

	// Verify both annotations exist
	dep, err = clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, dep.Annotations, approval.ApprovalsAnnotation, "approval should still be present")
	assert.Contains(t, dep.Annotations, approval.RejectionsAnnotation, "rejection should be present")

	// Step 7: User modifies RS (this is allowed - user modifications are NOT drift)
	t.Log("")
	t.Log("Step 7: User modifies ReplicaSet (this is NOT drift, should succeed)...")
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		rs, err = clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		rs.Spec.Replicas = ptr(int32(2))
		_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{})
		return err
	})
	require.NoError(t, err)
	t.Log("User modification succeeded (expected - user changes are new causal origin, not drift)")

	// Step 8: Wait and verify controller drift is blocked
	t.Log("")
	t.Log("Step 8: Waiting to verify controller drift is blocked by rejection...")
	t.Log("Controller will try to fix replicas back to 1, but rejection should block it.")

	// Give controller time to attempt reconciliation
	time.Sleep(3 * time.Second)

	// Verify RS still has our modification (controller couldn't fix it)
	rs, err = clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, int32(2), *rs.Spec.Replicas, "rejection should have blocked controller drift")

	t.Log("")
	t.Log("SUCCESS: Rejection overrides approval and blocks controller drift")
	t.Log("- With approval only: controller drift was ALLOWED")
	t.Log("- With approval + rejection: controller drift BLOCKED (replicas stayed at 2)")
}

// TestEnforceModeBlocksDrift verifies the difference between log mode and enforce mode.
// This proves that mode annotation actually controls enforcement.
func TestEnforceModeBlocksDrift(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Enforce Mode vs Log Mode ===")
	t.Log("This test proves mode annotation controls enforcement:")
	t.Log("1. In log mode: drift is allowed (controller can fix)")
	t.Log("2. In enforce mode: drift is blocked (controller cannot fix)")

	// Part 1: Log mode (drift allowed)
	t.Log("")
	t.Log("=== PART 1: LOG MODE ===")

	logNS := fmt.Sprintf("log-mode-%s", rand.String(4))
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: logNS,
			Annotations: map[string]string{
				"kausality.io/mode": "log",
			},
		},
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting namespace %s", logNS)
		_ = clientset.CoreV1().Namespaces().Delete(ctx, logNS, metav1.DeleteOptions{})
	})
	t.Logf("Created namespace %s with log mode", logNS)

	// Create deployment in log mode namespace
	logName := fmt.Sprintf("log-mode-%s", rand.String(4))
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      logName,
			Namespace: logNS,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": logName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": logName},
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

	_, err = clientset.AppsV1().Deployments(logNS).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for stabilization
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(logNS).Get(ctx, logName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting deployment: %v", err)
		}
		if dep.Status.ObservedGeneration != dep.Generation || dep.Status.AvailableReplicas < 1 {
			return false, "not stable yet"
		}
		return true, "deployment stabilized"
	}, defaultTimeout, defaultInterval, "deployment should stabilize")

	// Get ReplicaSet
	var logRSName string
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(logNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", logName),
		})
		if err != nil || len(rsList.Items) == 0 {
			return false, "no replicaset found"
		}
		logRSName = rsList.Items[0].Name
		return true, "found replicaset"
	}, defaultTimeout, defaultInterval, "replicaset should exist")

	// Modify ReplicaSet in log mode
	rs, err := clientset.AppsV1().ReplicaSets(logNS).Get(ctx, logRSName, metav1.GetOptions{})
	require.NoError(t, err)
	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(logNS).Update(ctx, rs, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("Modified ReplicaSet replicas to 2 in log mode namespace")

	// In log mode, controller should fix it back
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(logNS).Get(ctx, logRSName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		if *rs.Spec.Replicas != 1 {
			return false, fmt.Sprintf("replicas=%d, waiting for controller", *rs.Spec.Replicas)
		}
		return true, "controller fixed replicas (drift allowed in log mode)"
	}, defaultTimeout, defaultInterval, "drift should be allowed in log mode")
	t.Log("LOG MODE: Controller fixed replicas back to 1 (drift allowed)")

	// Part 2: Enforce mode (drift blocked)
	t.Log("")
	t.Log("=== PART 2: ENFORCE MODE ===")

	enforceNS := fmt.Sprintf("enforce-mode-%s", rand.String(4))
	ns = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: enforceNS,
			Annotations: map[string]string{
				"kausality.io/mode": "enforce",
			},
		},
	}
	_, err = clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting namespace %s", enforceNS)
		_ = clientset.CoreV1().Namespaces().Delete(ctx, enforceNS, metav1.DeleteOptions{})
	})
	t.Logf("Created namespace %s with enforce mode", enforceNS)

	// Create fresh deployment in enforce mode namespace (don't reuse Part 1's object)
	enforceName := fmt.Sprintf("enforce-mode-%s", rand.String(4))
	enforceDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      enforceName,
			Namespace: enforceNS,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": enforceName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": enforceName},
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

	_, err = clientset.AppsV1().Deployments(enforceNS).Create(ctx, enforceDeployment, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for stabilization
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(enforceNS).Get(ctx, enforceName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting deployment: %v", err)
		}
		if dep.Status.ObservedGeneration != dep.Generation || dep.Status.AvailableReplicas < 1 {
			return false, "not stable yet"
		}
		return true, "deployment stabilized"
	}, defaultTimeout, defaultInterval, "deployment should stabilize")

	// Get ReplicaSet
	var enforceRSName string
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(enforceNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", enforceName),
		})
		if err != nil || len(rsList.Items) == 0 {
			return false, "no replicaset found"
		}
		enforceRSName = rsList.Items[0].Name
		return true, "found replicaset"
	}, defaultTimeout, defaultInterval, "replicaset should exist")

	// Modify ReplicaSet in enforce mode
	rs, err = clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, enforceRSName, metav1.GetOptions{})
	require.NoError(t, err)
	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("Modified ReplicaSet replicas to 2 in enforce mode namespace")

	// In enforce mode, modification should persist (controller blocked)
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, enforceRSName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		if *rs.Spec.Replicas != 2 {
			return false, fmt.Sprintf("replicas changed to %d (drift allowed in enforce mode!)", *rs.Spec.Replicas)
		}
		return true, "replicas still 2 (drift blocked in enforce mode)"
	}, defaultTimeout, defaultInterval, "drift should be blocked in enforce mode")
	t.Log("ENFORCE MODE: Replicas stayed at 2 (drift blocked)")

	t.Log("")
	t.Log("SUCCESS: Mode annotation controls enforcement")
	t.Log("- Log mode: drift allowed, controller fixed replicas to 1")
	t.Log("- Enforce mode: drift blocked, replicas stayed at 2")
}

// TestFreezeBlocksAllMutations verifies that kausality.io/freeze annotation
// blocks ALL child mutations, even when there is an approval.
func TestFreezeBlocksAllMutations(t *testing.T) {
	ctx := context.Background()

	t.Log("=== Testing Freeze Blocks All Mutations ===")
	t.Log("When a parent has freeze=true, ALL child mutations are blocked,")
	t.Log("even if there is an approval annotation. This is an emergency lockdown.")
	t.Log("")
	t.Log("This test proves freeze makes a difference by showing:")
	t.Log("1. With approval alone, drift IS allowed (counter-example)")
	t.Log("2. With approval + freeze, drift is BLOCKED (freeze wins)")

	// Step 1: Create a namespace with enforce mode
	enforceNS := fmt.Sprintf("freeze-test-%s", rand.String(4))
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
	t.Logf("Created namespace %s with enforce mode", enforceNS)

	// Step 2: Create a Deployment WITH approval for all ReplicaSets
	t.Log("")
	t.Log("Step 2: Creating Deployment WITH approval annotation...")
	name := fmt.Sprintf("freeze-test-%s", rand.String(4))

	approvals := []approval.Approval{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "*", // Approve all ReplicaSets
		Mode:       approval.ModeAlways,
	}}
	approvalData, err := json.Marshal(approvals)
	require.NoError(t, err)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: enforceNS,
			Annotations: map[string]string{
				approval.ApprovalsAnnotation: string(approvalData),
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
						Image: "nginx:1.24-alpine",
					}},
				},
			},
		},
	}

	_, err = clientset.AppsV1().Deployments(enforceNS).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Logf("Created Deployment %s with approval annotation", name)

	// Step 3: Wait for stabilization
	ktesting.Eventually(t, func() (bool, string) {
		dep, err := clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
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

	// Step 4: Get the ReplicaSet name
	var rsName string
	ktesting.Eventually(t, func() (bool, string) {
		rsList, err := clientset.AppsV1().ReplicaSets(enforceNS).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if err != nil {
			return false, fmt.Sprintf("error listing replicasets: %v", err)
		}
		if len(rsList.Items) == 0 {
			return false, "no replicaset found"
		}
		rsName = rsList.Items[0].Name
		return true, fmt.Sprintf("found replicaset %s", rsName)
	}, defaultTimeout, defaultInterval, "replicaset should exist")

	// Step 5: COUNTER-EXAMPLE - Verify approval allows drift
	t.Log("")
	t.Log("Step 5: COUNTER-EXAMPLE - Verify approval allows drift...")
	t.Log("Modifying ReplicaSet replicas from 1 to 2...")

	rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
	require.NoError(t, err)
	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("Modified ReplicaSet spec.replicas to 2")

	// Wait for controller to fix it back (should be allowed with approval)
	ktesting.Eventually(t, func() (bool, string) {
		rs, err := clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error getting replicaset: %v", err)
		}
		if *rs.Spec.Replicas != 1 {
			return false, fmt.Sprintf("replicas still %d, waiting for controller to fix it", *rs.Spec.Replicas)
		}
		return true, "controller fixed replicas back to 1 (drift allowed with approval)"
	}, defaultTimeout, defaultInterval, "approval should allow drift")
	t.Log("COUNTER-EXAMPLE PASSED: With approval alone, controller fixed the drift")

	// Step 6: Add freeze annotation (keeping approval)
	t.Log("")
	t.Log("Step 6: Adding freeze annotation (keeping approval)...")
	dep, err := clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	dep.Annotations["kausality.io/freeze"] = "true"
	_, err = clientset.AppsV1().Deployments(enforceNS).Update(ctx, dep, metav1.UpdateOptions{})
	require.NoError(t, err)
	t.Log("Added freeze=true annotation (approval still present)")

	// Verify both annotations exist
	dep, err = clientset.AppsV1().Deployments(enforceNS).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, dep.Annotations, approval.ApprovalsAnnotation, "approval should still be present")
	assert.Equal(t, "true", dep.Annotations["kausality.io/freeze"], "freeze should be set")

	// Step 7: Verify freeze blocks ALL mutations (including user modifications)
	t.Log("")
	t.Log("Step 7: Attempting to modify ReplicaSet - freeze should block ALL mutations...")
	rs, err = clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
	require.NoError(t, err)
	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{})
	require.Error(t, err, "freeze should block user modification")
	assert.True(t, strings.Contains(err.Error(), "frozen"),
		"error should contain 'frozen', got: %s", err.Error())
	t.Logf("User modification blocked as expected: %v", err)

	// Step 8: Verify controller drift is also blocked
	t.Log("")
	t.Log("Step 8: Verifying controller modifications are also blocked...")
	rs, err = clientset.AppsV1().ReplicaSets(enforceNS).Get(ctx, rsName, metav1.GetOptions{})
	require.NoError(t, err)
	rs.Spec.Replicas = ptr(int32(2))
	_, err = clientset.AppsV1().ReplicaSets(enforceNS).Update(ctx, rs, metav1.UpdateOptions{
		FieldManager: "deployment-controller",
	})
	require.Error(t, err, "freeze should block controller modification")
	errMsg := err.Error()
	t.Logf("Controller modification blocked: %s", errMsg)
	assert.True(t, strings.Contains(errMsg, "frozen"),
		"freeze error should contain 'frozen', got: %s", errMsg)

	t.Log("")
	t.Log("SUCCESS: Freeze overrides approval and blocks ALL mutations")
	t.Log("- With approval only: drift was ALLOWED (controller fixed replicas)")
	t.Log("- With freeze + approval: ALL changes BLOCKED (user and controller)")
	t.Log("- Freeze error message contains 'frozen'")
}
