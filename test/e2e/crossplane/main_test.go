//go:build e2e

package crossplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
)

const (
	kausalityNS       = "kausality-system"
	crossplaneNS      = "crossplane-system"
	defaultTimeout    = 2 * time.Minute
	defaultInterval   = 2 * time.Second
	annotationTimeout = 30 * time.Second
)

var (
	clientset       *kubernetes.Clientset
	dynamicClient   dynamic.Interface
	kausalityClient client.Client
	testNamespace   string

	// GVR for NopResource
	nopResourceGVR = schema.GroupVersionResource{
		Group:    "nop.crossplane.io",
		Version:  "v1alpha1",
		Resource: "nopresources",
	}

	// GVR for Provider
	providerGVR = schema.GroupVersionResource{
		Group:    "pkg.crossplane.io",
		Version:  "v1",
		Resource: "providers",
	}
)

func TestMain(m *testing.M) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Printf("Failed to load kubeconfig: %v\n", err)
		os.Exit(1)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("Failed to create clientset: %v\n", err)
		os.Exit(1)
	}

	dynamicClient, err = dynamic.NewForConfig(config)
	if err != nil {
		fmt.Printf("Failed to create dynamic client: %v\n", err)
		os.Exit(1)
	}

	// Create controller-runtime client for Kausality CRDs
	scheme := runtime.NewScheme()
	_ = kausalityv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	kausalityClient, err = client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Printf("Failed to create kausality client: %v\n", err)
		os.Exit(1)
	}

	// Generate unique namespace for this test run (reentrant)
	testNamespace = fmt.Sprintf("crossplane-test-%s", rand.String(6))
	fmt.Printf("Using test namespace: %s\n", testNamespace)

	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
	}
	_, err = clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Failed to create test namespace: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	// Cleanup namespace
	_ = clientset.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})

	os.Exit(code)
}

// makeNopResource creates an unstructured NopResource with the given name and optional annotations.
// NopResource is cluster-scoped in Crossplane, so no namespace is set.
// Note: kausality.io/trace-* metadata must be annotations, not labels.
func makeNopResource(name string, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "nop.crossplane.io/v1alpha1",
			"kind":       "NopResource",
			"metadata": map[string]interface{}{
				"name": name,
			},
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
	}
	if annotations != nil {
		obj.SetAnnotations(annotations)
	}
	return obj
}
