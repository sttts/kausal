//go:build envtest
// +build envtest

package admission_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/kausality-io/kausality/pkg/admission"
)

// Shared test environments
var (
	// Environment WITH webhook - for TestWebhook_* tests
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	testNS    string

	// Environment WITHOUT webhook - for unit tests that call handler directly
	cfgUnit       *rest.Config
	k8sClientUnit client.Client
	testEnvUnit   *envtest.Environment
	testNSUnit    string

	scheme = runtime.NewScheme()
	ctx    context.Context
	cancel context.CancelFunc
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())

	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	// Start environment WITHOUT webhook (for unit tests)
	if err := startUnitEnv(); err != nil {
		panic(fmt.Sprintf("failed to start unit env: %v", err))
	}

	// Start environment WITH webhook (for webhook tests)
	if err := startWebhookEnv(); err != nil {
		panic(fmt.Sprintf("failed to start webhook env: %v", err))
	}

	code := m.Run()

	// Cleanup
	cancel()
	_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}})
	_ = k8sClientUnit.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNSUnit}})
	_ = testEnv.Stop()
	_ = testEnvUnit.Stop()

	os.Exit(code)
}

// startUnitEnv starts envtest WITHOUT webhook for unit tests
func startUnitEnv() error {
	testEnvUnit = &envtest.Environment{}

	var err error
	cfgUnit, err = testEnvUnit.Start()
	if err != nil {
		return fmt.Errorf("failed to start envtest (unit): %w", err)
	}

	k8sClientUnit, err = client.New(cfgUnit, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("failed to create client (unit): %w", err)
	}

	// Create namespace for unit tests
	testNSUnit = fmt.Sprintf("kausality-unit-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNSUnit}}
	if err := k8sClientUnit.Create(ctx, ns); err != nil {
		return fmt.Errorf("failed to create test namespace (unit): %w", err)
	}

	return nil
}

// startWebhookEnv starts envtest WITH webhook for webhook tests
func startWebhookEnv() error {
	// Build mutating webhook configuration
	failPolicy := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	matchPolicy := admissionv1.Equivalent
	webhookPath := "/mutate"

	mutatingWebhook := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kausality-webhook",
		},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name:                    "mutate.admission.kausality.io",
				AdmissionReviewVersions: []string{"v1"},
				SideEffects:             &sideEffects,
				FailurePolicy:           &failPolicy,
				MatchPolicy:             &matchPolicy,
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Path: &webhookPath,
					},
				},
				Rules: []admissionv1.RuleWithOperations{
					{
						Operations: []admissionv1.OperationType{
							admissionv1.Create,
							admissionv1.Update,
							admissionv1.Delete,
						},
						Rule: admissionv1.Rule{
							APIGroups:   []string{"apps"},
							APIVersions: []string{"v1"},
							Resources:   []string{"deployments", "replicasets", "statefulsets", "daemonsets"},
						},
					},
					{
						Operations: []admissionv1.OperationType{
							admissionv1.Update,
						},
						Rule: admissionv1.Rule{
							APIGroups:   []string{"apps"},
							APIVersions: []string{"v1"},
							Resources:   []string{"deployments/status", "replicasets/status", "statefulsets/status", "daemonsets/status"},
						},
					},
					{
						Operations: []admissionv1.OperationType{
							admissionv1.Create,
							admissionv1.Update,
							admissionv1.Delete,
						},
						Rule: admissionv1.Rule{
							APIGroups:   []string{""},
							APIVersions: []string{"v1"},
							Resources:   []string{"configmaps", "secrets", "services"},
						},
					},
				},
			},
		},
	}

	testEnv = &envtest.Environment{
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			MutatingWebhooks: []*admissionv1.MutatingWebhookConfiguration{mutatingWebhook},
		},
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		return fmt.Errorf("failed to start envtest (webhook): %w", err)
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("failed to create client (webhook): %w", err)
	}

	// Start webhook server
	webhookInstallOptions := &testEnv.WebhookInstallOptions
	webhookServer := webhook.NewServer(webhook.Options{
		Host:    webhookInstallOptions.LocalServingHost,
		Port:    webhookInstallOptions.LocalServingPort,
		CertDir: webhookInstallOptions.LocalServingCertDir,
	})

	// Create and register the admission handler
	handler := admission.NewHandler(admission.Config{
		Client: k8sClient,
		Log:    ctrl.Log,
	})
	webhookServer.Register("/mutate", &webhook.Admission{Handler: handler})

	// Start webhook server in background
	go func() {
		if err := webhookServer.Start(ctx); err != nil {
			panic(fmt.Sprintf("failed to start webhook server: %v", err))
		}
	}()

	// Wait for webhook server to be ready
	dialer := &net.Dialer{Timeout: time.Second}
	addrPort := fmt.Sprintf("%s:%d", webhookInstallOptions.LocalServingHost, webhookInstallOptions.LocalServingPort)

	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

waitLoop:
	for {
		select {
		case <-timeout:
			return fmt.Errorf("timed out waiting for webhook server")
		case <-ticker.C:
			conn, err := tls.DialWithDialer(dialer, "tcp", addrPort, &tls.Config{InsecureSkipVerify: true})
			if err == nil {
				conn.Close()
				break waitLoop
			}
		}
	}

	// Create namespace for webhook tests
	testNS = fmt.Sprintf("kausality-webhook-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		return fmt.Errorf("failed to create test namespace (webhook): %w", err)
	}

	return nil
}
