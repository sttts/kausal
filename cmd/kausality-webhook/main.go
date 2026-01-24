// Command kausality-webhook runs the drift detection webhook server.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/kausality-io/kausality/pkg/webhook"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var (
		host                   string
		port                   int
		certDir                string
		healthProbeBindAddress string
	)

	flag.StringVar(&host, "host", "", "The address to bind to (default: all interfaces)")
	flag.IntVar(&port, "port", 9443, "The port to listen on for webhook requests")
	flag.StringVar(&certDir, "cert-dir", "/etc/webhook/certs", "The directory containing tls.crt and tls.key")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "The address for health probes")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	log := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(log)

	log.Info("starting kausality-webhook",
		"host", host,
		"port", port,
		"certDir", certDir,
		"healthProbeBindAddress", healthProbeBindAddress,
	)

	// Create Kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Error(err, "unable to get in-cluster config, trying kubeconfig")
		config, err = ctrl.GetConfig()
		if err != nil {
			log.Error(err, "unable to get kubeconfig")
			os.Exit(1)
		}
	}

	k8sClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	// Create and start webhook server
	server := webhook.NewServer(webhook.Config{
		Client:                 k8sClient,
		Log:                    log,
		Host:                   host,
		Port:                   port,
		CertDir:                certDir,
		HealthProbeBindAddress: healthProbeBindAddress,
	})

	server.Register()

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go handleSignals(ctx, cancel, log)

	if err := server.Start(ctx); err != nil {
		log.Error(err, "webhook server failed")
		os.Exit(1)
	}
}

func handleSignals(ctx context.Context, cancel context.CancelFunc, log logr.Logger) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info("received signal, shutting down", "signal", sig)
		cancel()
	case <-ctx.Done():
	}
}
