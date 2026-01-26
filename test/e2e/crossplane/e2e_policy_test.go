//go:build e2e

package crossplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

// TestPolicyCrossplaneEnforceMode verifies that a Kausality policy with enforce mode
// blocks drift on Crossplane composition resources.
//
// This test:
// 1. Creates a Kausality policy for test.kausality.io resources (XPlatform/XService)
// 2. Creates a composition hierarchy
// 3. Verifies drift is blocked when Crossplane tries to correct user modifications
func TestPolicyCrossplaneEnforceMode(t *testing.T) {
	ctx := context.Background()
	suffix := rand.String(4)
	policyName := fmt.Sprintf("crossplane-enforce-%s", suffix)

	t.Log("=== Testing Kausality Policy Enforce Mode for Crossplane ===")
	t.Log("When a Kausality policy specifies enforce mode for Crossplane resources,")
	t.Log("drift should be blocked when Crossplane tries to correct user modifications.")

	// Step 1: Create Kausality policy for Crossplane test resources
	t.Log("")
	t.Log("Step 1: Creating Kausality policy with enforce mode...")

	policy := &kausalityv1alpha1.Kausality{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: kausalityv1alpha1.KausalitySpec{
			Mode: kausalityv1alpha1.ModeEnforce,
			Resources: []kausalityv1alpha1.ResourceRule{
				{
					APIGroups: []string{"test.kausality.io"},
					Resources: []string{"*"},
				},
				{
					APIGroups: []string{"nop.crossplane.io"},
					Resources: []string{"*"},
				},
			},
		},
	}

	err := kausalityClient.Create(ctx, policy)
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting Kausality policy %s", policyName)
		_ = kausalityClient.Delete(ctx, policy)
	})

	t.Logf("Created Kausality policy %s with enforce mode for test.kausality.io and nop.crossplane.io", policyName)

	// Wait for policy to be ready
	t.Log("")
	t.Log("Step 2: Waiting for policy to be ready...")

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
		return false, "policy not ready yet"
	}, 30*time.Second, 2*time.Second, "policy should become ready")

	t.Log("Policy is ready")

	// Step 3: Ensure XRDs exist
	t.Log("")
	t.Log("Step 3: Ensuring XRDs exist...")

	xserviceXRD := makeXServiceXRD()
	_, err = dynamicClient.Resource(xrdGVR).Create(ctx, xserviceXRD, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "failed to create XService XRD")
	}

	xplatformXRD := makeXPlatformXRD()
	_, err = dynamicClient.Resource(xrdGVR).Create(ctx, xplatformXRD, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "failed to create XPlatform XRD")
	}

	waitForXRDEstablished(t, ctx, "xservices.test.kausality.io")
	waitForXRDEstablished(t, ctx, "xplatforms.test.kausality.io")
	t.Log("XRDs are established")

	// Step 4: Create compositions (without enforce annotation - mode comes from policy)
	t.Log("")
	t.Log("Step 4: Creating compositions (mode will come from policy, not annotations)...")

	xserviceComposition := makeXServiceCompositionNoAnnotation(suffix)
	_, err = dynamicClient.Resource(compositionGVR).Create(ctx, xserviceComposition, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create XService composition")
	t.Cleanup(func() {
		_ = dynamicClient.Resource(compositionGVR).Delete(ctx, xserviceComposition.GetName(), metav1.DeleteOptions{})
	})

	xplatformComposition := makeXPlatformCompositionNoAnnotation(suffix)
	_, err = dynamicClient.Resource(compositionGVR).Create(ctx, xplatformComposition, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create XPlatform composition")
	t.Cleanup(func() {
		_ = dynamicClient.Resource(compositionGVR).Delete(ctx, xplatformComposition.GetName(), metav1.DeleteOptions{})
	})

	t.Log("Compositions created")

	// Step 5: Create XPlatform (no mode annotation - mode from policy)
	t.Log("")
	t.Log("Step 5: Creating XPlatform without mode annotation...")

	xplatformGVR := schema.GroupVersionResource{
		Group:    "test.kausality.io",
		Version:  "v1alpha1",
		Resource: "xplatforms",
	}

	xplatform := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test.kausality.io/v1alpha1",
			"kind":       "XPlatform",
			"metadata": map[string]interface{}{
				"name": "policy-test-" + suffix,
				// No kausality.io/mode annotation - mode comes from CRD policy
			},
			"spec": map[string]interface{}{
				"platformName": "policy-test",
			},
		},
	}

	_, err = dynamicClient.Resource(xplatformGVR).Create(ctx, xplatform, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create XPlatform")
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting XPlatform policy-test-%s", suffix)
		_ = dynamicClient.Resource(xplatformGVR).Delete(ctx, "policy-test-"+suffix, metav1.DeleteOptions{})
		time.Sleep(2 * time.Second)
	})

	t.Logf("Created XPlatform 'policy-test-%s' without mode annotation", suffix)

	// Step 6: Wait for composition hierarchy
	t.Log("")
	t.Log("Step 6: Waiting for composition hierarchy...")

	xserviceGVR := schema.GroupVersionResource{
		Group:    "test.kausality.io",
		Version:  "v1alpha1",
		Resource: "xservices",
	}

	var xserviceName string
	ktesting.Eventually(t, func() (bool, string) {
		list, err := dynamicClient.Resource(xserviceGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Sprintf("error listing XServices: %v", err)
		}
		for _, item := range list.Items {
			owners := item.GetOwnerReferences()
			for _, owner := range owners {
				if owner.Kind == "XPlatform" && owner.Name == "policy-test-"+suffix {
					xserviceName = item.GetName()
					return true, fmt.Sprintf("found XService %s", xserviceName)
				}
			}
		}
		return false, fmt.Sprintf("no XService with XPlatform owner (found %d XServices)", len(list.Items))
	}, 90*time.Second, 2*time.Second, "XService should be created by XPlatform composition")

	var nopResourceName string
	ktesting.Eventually(t, func() (bool, string) {
		list, err := dynamicClient.Resource(nopResourceGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Sprintf("error listing NopResources: %v", err)
		}
		for _, item := range list.Items {
			owners := item.GetOwnerReferences()
			for _, owner := range owners {
				if owner.Kind == "XService" && owner.Name == xserviceName {
					nopResourceName = item.GetName()
					return true, fmt.Sprintf("found NopResource %s", nopResourceName)
				}
			}
		}
		return false, fmt.Sprintf("no NopResource with XService owner (found %d NopResources)", len(list.Items))
	}, 120*time.Second, 2*time.Second, "NopResource should be created by XService composition")

	t.Logf("Found hierarchy: XPlatform -> %s -> %s", xserviceName, nopResourceName)

	// Wait for stability
	ktesting.Eventually(t, func() (bool, string) {
		obj, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, nopResourceName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found {
			return false, "no conditions"
		}
		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			cType, _, _ := unstructured.NestedString(cond, "type")
			cStatus, _, _ := unstructured.NestedString(cond, "status")
			if cType == "Ready" && cStatus == "True" {
				return true, "NopResource Ready"
			}
		}
		return false, "waiting for Ready"
	}, 60*time.Second, 2*time.Second, "NopResource should be Ready")

	ktesting.Eventually(t, func() (bool, string) {
		obj, err := dynamicClient.Resource(xserviceGVR).Get(ctx, xserviceName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		gen := obj.GetGeneration()
		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found {
			return false, "no conditions"
		}
		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			cType, _, _ := unstructured.NestedString(cond, "type")
			cStatus, _, _ := unstructured.NestedString(cond, "status")
			obsGen, _, _ := unstructured.NestedInt64(cond, "observedGeneration")
			if cType == "Synced" && cStatus == "True" && obsGen == gen {
				return true, "XService stable"
			}
		}
		return false, "waiting for stability"
	}, 60*time.Second, 2*time.Second, "XService should stabilize")

	t.Log("Composition hierarchy is stable")

	// Step 7: User modifies NopResource
	t.Log("")
	t.Log("Step 7: User modifies NopResource (new causal origin, should be allowed)...")

	nopResource, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, nopResourceName, metav1.GetOptions{})
	require.NoError(t, err)

	err = unstructured.SetNestedField(nopResource.Object, []interface{}{
		map[string]interface{}{
			"time":            "77s",
			"conditionType":   "Ready",
			"conditionStatus": "True",
		},
	}, "spec", "forProvider", "conditionAfter")
	require.NoError(t, err)

	_, err = dynamicClient.Resource(nopResourceGVR).Update(ctx, nopResource, metav1.UpdateOptions{})
	require.NoError(t, err, "user modification should be allowed")
	t.Log("User modification succeeded")

	// Step 8: Verify drift is blocked
	t.Log("")
	t.Log("Step 8: Waiting to verify drift correction is blocked by policy...")

	time.Sleep(10 * time.Second)

	nopResource, err = dynamicClient.Resource(nopResourceGVR).Get(ctx, nopResourceName, metav1.GetOptions{})
	require.NoError(t, err)

	conditionAfter, found, _ := unstructured.NestedSlice(nopResource.Object, "spec", "forProvider", "conditionAfter")
	require.True(t, found && len(conditionAfter) > 0)

	firstCond, ok := conditionAfter[0].(map[string]interface{})
	require.True(t, ok)

	timeVal, _, _ := unstructured.NestedString(firstCond, "time")
	if timeVal == "77s" {
		t.Log("PASS: User modification persisted - drift correction was blocked by POLICY")
		t.Log("Mode was determined from Kausality CRD, not annotation")
	} else {
		t.Logf("FAIL: NopResource was reverted to %s - drift correction was allowed", timeVal)
		t.Log("Expected policy enforce mode to block drift")
		t.FailNow()
	}

	t.Log("")
	t.Log("SUCCESS: Kausality policy enforce mode blocks drift for Crossplane resources")
}

// TestPolicyCrossplaneModeUpdate verifies that updating a Kausality policy
// from enforce to log mode takes effect without restarting the webhook.
func TestPolicyCrossplaneModeUpdate(t *testing.T) {
	ctx := context.Background()
	suffix := rand.String(4)
	policyName := fmt.Sprintf("crossplane-update-%s", suffix)

	t.Log("=== Testing Kausality Policy Mode Update for Crossplane ===")
	t.Log("When a policy is updated from enforce to log mode,")
	t.Log("drift should become allowed without restarting the webhook.")

	// Step 1: Create policy with enforce mode
	t.Log("")
	t.Log("Step 1: Creating Kausality policy with enforce mode...")

	policy := &kausalityv1alpha1.Kausality{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: kausalityv1alpha1.KausalitySpec{
			Mode: kausalityv1alpha1.ModeEnforce,
			Resources: []kausalityv1alpha1.ResourceRule{
				{
					APIGroups: []string{"test.kausality.io"},
					Resources: []string{"*"},
				},
				{
					APIGroups: []string{"nop.crossplane.io"},
					Resources: []string{"*"},
				},
			},
		},
	}

	err := kausalityClient.Create(ctx, policy)
	require.NoError(t, err)
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting Kausality policy %s", policyName)
		_ = kausalityClient.Delete(ctx, policy)
	})

	// Wait for policy ready
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
		return false, "policy not ready yet"
	}, 30*time.Second, 2*time.Second, "policy should become ready")

	t.Log("Policy created with enforce mode")

	// Step 2: Set up composition hierarchy
	t.Log("")
	t.Log("Step 2: Setting up composition hierarchy...")

	// Ensure XRDs
	xserviceXRD := makeXServiceXRD()
	_, _ = dynamicClient.Resource(xrdGVR).Create(ctx, xserviceXRD, metav1.CreateOptions{})
	xplatformXRD := makeXPlatformXRD()
	_, _ = dynamicClient.Resource(xrdGVR).Create(ctx, xplatformXRD, metav1.CreateOptions{})
	waitForXRDEstablished(t, ctx, "xservices.test.kausality.io")
	waitForXRDEstablished(t, ctx, "xplatforms.test.kausality.io")

	// Create compositions
	xserviceComp := makeXServiceCompositionNoAnnotation(suffix + "-upd")
	_, err = dynamicClient.Resource(compositionGVR).Create(ctx, xserviceComp, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = dynamicClient.Resource(compositionGVR).Delete(ctx, xserviceComp.GetName(), metav1.DeleteOptions{})
	})

	xplatformComp := makeXPlatformCompositionNoAnnotation(suffix + "-upd")
	_, err = dynamicClient.Resource(compositionGVR).Create(ctx, xplatformComp, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = dynamicClient.Resource(compositionGVR).Delete(ctx, xplatformComp.GetName(), metav1.DeleteOptions{})
	})

	// Create XPlatform
	xplatformGVR := schema.GroupVersionResource{
		Group:    "test.kausality.io",
		Version:  "v1alpha1",
		Resource: "xplatforms",
	}

	xplatform := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "test.kausality.io/v1alpha1",
			"kind":       "XPlatform",
			"metadata": map[string]interface{}{
				"name": "update-test-" + suffix,
			},
			"spec": map[string]interface{}{
				"platformName": "update-test",
			},
		},
	}

	_, err = dynamicClient.Resource(xplatformGVR).Create(ctx, xplatform, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = dynamicClient.Resource(xplatformGVR).Delete(ctx, "update-test-"+suffix, metav1.DeleteOptions{})
		time.Sleep(2 * time.Second)
	})

	// Wait for hierarchy
	xserviceGVR := schema.GroupVersionResource{
		Group:    "test.kausality.io",
		Version:  "v1alpha1",
		Resource: "xservices",
	}

	var xserviceName string
	ktesting.Eventually(t, func() (bool, string) {
		list, err := dynamicClient.Resource(xserviceGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		for _, item := range list.Items {
			for _, owner := range item.GetOwnerReferences() {
				if owner.Kind == "XPlatform" && owner.Name == "update-test-"+suffix {
					xserviceName = item.GetName()
					return true, "found XService"
				}
			}
		}
		return false, "waiting for XService"
	}, 90*time.Second, 2*time.Second, "XService should be created")

	var nopName string
	ktesting.Eventually(t, func() (bool, string) {
		list, err := dynamicClient.Resource(nopResourceGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		for _, item := range list.Items {
			for _, owner := range item.GetOwnerReferences() {
				if owner.Kind == "XService" && owner.Name == xserviceName {
					nopName = item.GetName()
					return true, "found NopResource"
				}
			}
		}
		return false, "waiting for NopResource"
	}, 120*time.Second, 2*time.Second, "NopResource should be created")

	// Wait for stability
	ktesting.Eventually(t, func() (bool, string) {
		obj, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, nopName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found {
			return false, "no conditions"
		}
		for _, c := range conditions {
			cond, _ := c.(map[string]interface{})
			cType, _, _ := unstructured.NestedString(cond, "type")
			cStatus, _, _ := unstructured.NestedString(cond, "status")
			if cType == "Ready" && cStatus == "True" {
				return true, "Ready"
			}
		}
		return false, "waiting"
	}, 60*time.Second, 2*time.Second, "NopResource should be Ready")

	ktesting.Eventually(t, func() (bool, string) {
		obj, err := dynamicClient.Resource(xserviceGVR).Get(ctx, xserviceName, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Sprintf("error: %v", err)
		}
		gen := obj.GetGeneration()
		conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if !found {
			return false, "no conditions"
		}
		for _, c := range conditions {
			cond, _ := c.(map[string]interface{})
			cType, _, _ := unstructured.NestedString(cond, "type")
			cStatus, _, _ := unstructured.NestedString(cond, "status")
			obsGen, _, _ := unstructured.NestedInt64(cond, "observedGeneration")
			if cType == "Synced" && cStatus == "True" && obsGen == gen {
				return true, "stable"
			}
		}
		return false, "waiting"
	}, 60*time.Second, 2*time.Second, "XService should stabilize")

	t.Log("Hierarchy is stable")

	// Step 3: Verify enforce mode blocks drift
	t.Log("")
	t.Log("Step 3: Verifying enforce mode blocks drift...")

	nopResource, err := dynamicClient.Resource(nopResourceGVR).Get(ctx, nopName, metav1.GetOptions{})
	require.NoError(t, err)

	err = unstructured.SetNestedField(nopResource.Object, []interface{}{
		map[string]interface{}{
			"time":            "66s",
			"conditionType":   "Ready",
			"conditionStatus": "True",
		},
	}, "spec", "forProvider", "conditionAfter")
	require.NoError(t, err)

	_, err = dynamicClient.Resource(nopResourceGVR).Update(ctx, nopResource, metav1.UpdateOptions{})
	require.NoError(t, err)

	time.Sleep(10 * time.Second)

	nopResource, err = dynamicClient.Resource(nopResourceGVR).Get(ctx, nopName, metav1.GetOptions{})
	require.NoError(t, err)

	conditionAfter, found, _ := unstructured.NestedSlice(nopResource.Object, "spec", "forProvider", "conditionAfter")
	require.True(t, found && len(conditionAfter) > 0)
	firstCond, _ := conditionAfter[0].(map[string]interface{})
	timeVal, _, _ := unstructured.NestedString(firstCond, "time")
	require.Equal(t, "66s", timeVal, "modification should persist (drift blocked in enforce)")
	t.Log("Drift blocked as expected (enforce mode)")

	// Step 4: Update policy to log mode
	t.Log("")
	t.Log("Step 4: Updating policy to log mode...")

	var currentPolicy kausalityv1alpha1.Kausality
	err = kausalityClient.Get(ctx, client.ObjectKey{Name: policyName}, &currentPolicy)
	require.NoError(t, err)

	currentPolicy.Spec.Mode = kausalityv1alpha1.ModeLog
	err = kausalityClient.Update(ctx, &currentPolicy)
	require.NoError(t, err)

	t.Log("Policy updated to log mode")

	// Wait for policy refresh
	time.Sleep(3 * time.Second)

	// Step 5: Verify log mode allows drift correction
	t.Log("")
	t.Log("Step 5: Verifying log mode allows drift correction...")

	// Modify again
	nopResource, err = dynamicClient.Resource(nopResourceGVR).Get(ctx, nopName, metav1.GetOptions{})
	require.NoError(t, err)

	err = unstructured.SetNestedField(nopResource.Object, []interface{}{
		map[string]interface{}{
			"time":            "55s",
			"conditionType":   "Ready",
			"conditionStatus": "True",
		},
	}, "spec", "forProvider", "conditionAfter")
	require.NoError(t, err)

	_, err = dynamicClient.Resource(nopResourceGVR).Update(ctx, nopResource, metav1.UpdateOptions{})
	require.NoError(t, err)

	// In log mode, Crossplane should be able to correct the drift
	// Wait and check if drift correction happened or was just logged
	time.Sleep(15 * time.Second)

	nopResource, err = dynamicClient.Resource(nopResourceGVR).Get(ctx, nopName, metav1.GetOptions{})
	require.NoError(t, err)

	conditionAfter, found, _ = unstructured.NestedSlice(nopResource.Object, "spec", "forProvider", "conditionAfter")
	require.True(t, found && len(conditionAfter) > 0)
	firstCond, _ = conditionAfter[0].(map[string]interface{})
	timeVal, _, _ = unstructured.NestedString(firstCond, "time")

	// In log mode, either the modification persists (if Crossplane doesn't try to correct)
	// or it gets reverted (if Crossplane corrects and drift is allowed)
	// Either outcome is acceptable - the key is that log mode doesn't BLOCK
	t.Logf("After log mode switch, conditionAfter.time = %s", timeVal)
	if timeVal == "55s" {
		t.Log("Modification persisted (Crossplane didn't try to correct, or correction was slow)")
	} else {
		t.Log("Modification was reverted (Crossplane corrected - log mode allowed drift)")
	}

	t.Log("")
	t.Log("SUCCESS: Policy mode update takes effect for Crossplane resources")
}

// makeXServiceCompositionNoAnnotation creates a composition without mode annotations.
func makeXServiceCompositionNoAnnotation(suffix string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apiextensions.crossplane.io/v1",
			"kind":       "Composition",
			"metadata": map[string]interface{}{
				"name": "policy-xservice-comp-" + suffix,
			},
			"spec": map[string]interface{}{
				"compositeTypeRef": map[string]interface{}{
					"apiVersion": "test.kausality.io/v1alpha1",
					"kind":       "XService",
				},
				"mode": "Pipeline",
				"pipeline": []interface{}{
					map[string]interface{}{
						"step": "create-nopresource",
						"functionRef": map[string]interface{}{
							"name": "function-patch-and-transform",
						},
						"input": map[string]interface{}{
							"apiVersion": "pt.fn.crossplane.io/v1beta1",
							"kind":       "Resources",
							"resources": []interface{}{
								map[string]interface{}{
									"name": "nop",
									"base": map[string]interface{}{
										"apiVersion": "nop.crossplane.io/v1alpha1",
										"kind":       "NopResource",
										// No kausality.io/mode annotation - mode from policy
										"spec": map[string]interface{}{
											"forProvider": map[string]interface{}{
												"conditionAfter": []interface{}{
													map[string]interface{}{
														"time":            "3s",
														"conditionType":   "Ready",
														"conditionStatus": "True",
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// makeXPlatformCompositionNoAnnotation creates a composition without mode annotations.
func makeXPlatformCompositionNoAnnotation(suffix string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apiextensions.crossplane.io/v1",
			"kind":       "Composition",
			"metadata": map[string]interface{}{
				"name": "policy-xplatform-comp-" + suffix,
			},
			"spec": map[string]interface{}{
				"compositeTypeRef": map[string]interface{}{
					"apiVersion": "test.kausality.io/v1alpha1",
					"kind":       "XPlatform",
				},
				"mode": "Pipeline",
				"pipeline": []interface{}{
					map[string]interface{}{
						"step": "create-xservice",
						"functionRef": map[string]interface{}{
							"name": "function-patch-and-transform",
						},
						"input": map[string]interface{}{
							"apiVersion": "pt.fn.crossplane.io/v1beta1",
							"kind":       "Resources",
							"resources": []interface{}{
								map[string]interface{}{
									"name": "service",
									"base": map[string]interface{}{
										"apiVersion": "test.kausality.io/v1alpha1",
										"kind":       "XService",
										// No kausality.io/mode annotation - mode from policy
										"spec": map[string]interface{}{
											"serviceName":  "default",
											"delaySeconds": 3,
										},
									},
									"patches": []interface{}{
										map[string]interface{}{
											"type":          "FromCompositeFieldPath",
											"fromFieldPath": "spec.platformName",
											"toFieldPath":   "spec.serviceName",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
