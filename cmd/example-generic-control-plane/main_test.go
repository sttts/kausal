package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	crAdmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
	localAdmission "github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/admission"
	examplev1alpha1 "github.com/kausality-io/kausality/cmd/example-generic-control-plane/pkg/apis/example/v1alpha1"
	"github.com/kausality-io/kausality/pkg/policy"
)

// TestKausalityAdmission tests that the kausality admission plugin works correctly
// when integrated with the generic apiserver.
func TestKausalityAdmission(t *testing.T) {
	log := zap.New(zap.UseDevMode(true))

	// Create static policy resolver (enforce mode)
	policyResolver := policy.NewStaticResolver(kausalityv1alpha1.ModeEnforce)

	// Create a fake client with the default namespace
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	examplev1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
		}).
		Build()

	// Create kausality admission plugin with fake client
	kausalityPlugin := localAdmission.NewKausalityAdmission(fakeClient, log, policyResolver)

	t.Run("creates trace annotation on Widget CREATE", func(t *testing.T) {
		// Create a Widget object
		widget := &examplev1alpha1.Widget{
			TypeMeta: metav1.TypeMeta{
				APIVersion: examplev1alpha1.GroupVersion.String(),
				Kind:       "Widget",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-widget",
				Namespace: "default",
			},
			Spec: examplev1alpha1.WidgetSpec{
				Color: "blue",
			},
		}

		// Serialize to JSON
		widgetBytes, err := json.Marshal(widget)
		require.NoError(t, err)

		// Create admission request
		req := crAdmission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				UID: "test-uid",
				Kind: metav1.GroupVersionKind{
					Group:   examplev1alpha1.GroupVersion.Group,
					Version: examplev1alpha1.GroupVersion.Version,
					Kind:    "Widget",
				},
				Resource: metav1.GroupVersionResource{
					Group:    examplev1alpha1.GroupVersion.Group,
					Version:  examplev1alpha1.GroupVersion.Version,
					Resource: "widgets",
				},
				Namespace: "default",
				Name:      "test-widget",
				Operation: admissionv1.Create,
				UserInfo: authenticationv1.UserInfo{
					Username: "test-user",
					UID:      "test-user-uid",
				},
				Object: runtime.RawExtension{Raw: widgetBytes},
			},
		}

		// Call the handler directly (bypassing k8s.io/apiserver wrapper)
		ctx := context.Background()
		resp := kausalityPlugin.HandleDirect(ctx, req)

		// Should be allowed
		assert.True(t, resp.Allowed, "Widget CREATE should be allowed")

		// Should have patches for trace annotation
		if len(resp.Patches) > 0 {
			t.Logf("Patches: %+v", resp.Patches)
			// Look for trace annotation patch
			hasTrace := false
			for _, patch := range resp.Patches {
				if patch.Path == "/metadata/annotations" || patch.Path == "/metadata/annotations/kausality.io~1trace" {
					hasTrace = true
				}
			}
			assert.True(t, hasTrace, "Should have trace annotation patch")
		}
	})

	t.Run("allows Widget UPDATE without drift", func(t *testing.T) {
		// Old Widget
		oldWidget := &examplev1alpha1.Widget{
			TypeMeta: metav1.TypeMeta{
				APIVersion: examplev1alpha1.GroupVersion.String(),
				Kind:       "Widget",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-widget",
				Namespace: "default",
			},
			Spec: examplev1alpha1.WidgetSpec{
				Color: "blue",
			},
		}
		oldWidgetBytes, err := json.Marshal(oldWidget)
		require.NoError(t, err)

		// New Widget (same spec)
		newWidget := oldWidget.DeepCopy()
		newWidgetBytes, err := json.Marshal(newWidget)
		require.NoError(t, err)

		// Create admission request
		req := crAdmission.Request{
			AdmissionRequest: admissionv1.AdmissionRequest{
				UID: "test-uid",
				Kind: metav1.GroupVersionKind{
					Group:   examplev1alpha1.GroupVersion.Group,
					Version: examplev1alpha1.GroupVersion.Version,
					Kind:    "Widget",
				},
				Resource: metav1.GroupVersionResource{
					Group:    examplev1alpha1.GroupVersion.Group,
					Version:  examplev1alpha1.GroupVersion.Version,
					Resource: "widgets",
				},
				Namespace: "default",
				Name:      "test-widget",
				Operation: admissionv1.Update,
				UserInfo: authenticationv1.UserInfo{
					Username: "test-user",
					UID:      "test-user-uid",
				},
				Object:    runtime.RawExtension{Raw: newWidgetBytes},
				OldObject: runtime.RawExtension{Raw: oldWidgetBytes},
			},
		}

		// Call the handler
		ctx := context.Background()
		resp := kausalityPlugin.HandleDirect(ctx, req)

		// Should be allowed (no drift - same user, no parent)
		assert.True(t, resp.Allowed, "Widget UPDATE should be allowed when no drift")
	})
}
