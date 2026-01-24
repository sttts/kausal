//go:build e2e

package kubernetes

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	kausalityNS       = "kausality-system"
	defaultTimeout    = 2 * time.Minute
	defaultInterval   = 2 * time.Second
	annotationTimeout = 30 * time.Second
)

var (
	clientset     *kubernetes.Clientset
	testNamespace string
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

	// Generate unique namespace for this test run (reentrant)
	testNamespace = fmt.Sprintf("e2e-test-%s", rand.String(6))
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
