// Command kausality-webhook runs the drift detection webhook server.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
	"github.com/kausality-io/kausality/cmd/kausality-webhook/pkg/webhook"
	"github.com/kausality-io/kausality/pkg/callback"
	"github.com/kausality-io/kausality/pkg/config"
	"github.com/kausality-io/kausality/pkg/policy"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kausalityv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		host                   string
		port                   int
		certDir                string
		healthProbeBindAddress string
		configFile             string
		metricsAddr            string
	)

	flag.StringVar(&host, "host", "", "The address to bind to (default: all interfaces)")
	flag.IntVar(&port, "port", 9443, "The port to listen on for webhook requests")
	flag.StringVar(&certDir, "cert-dir", "/etc/webhook/certs", "The directory containing tls.crt and tls.key")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "The address for health probes")
	flag.StringVar(&configFile, "config", "", "Path to config file (optional, for drift callbacks)")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082", "The address for metrics endpoint")

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
		"configFile", configFile,
	)

	// Create controller manager for watch-based policy updates
	mgr, err := manager.New(ctrl.GetConfigOrDie(), manager.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: "", // We use our own health server
	})
	if err != nil {
		log.Error(err, "unable to create controller manager")
		os.Exit(1)
	}

	// Load config (optional, for drift callbacks)
	var driftConfig *config.Config
	if configFile != "" {
		driftConfig, err = config.Load(configFile)
		if err != nil {
			log.Error(err, "unable to load config file", "path", configFile)
			os.Exit(1)
		}
		log.Info("loaded config",
			"backends", len(driftConfig.Backends),
		)
	} else {
		driftConfig = config.Default()
		log.Info("using default config (no config file specified)")
	}

	// Create multi-sender if backends are configured
	var callbackSender callback.ReportSender
	if len(driftConfig.Backends) > 0 {
		senderConfigs := make([]callback.SenderConfig, len(driftConfig.Backends))
		for i, backend := range driftConfig.Backends {
			senderConfigs[i] = callback.SenderConfig{
				URL:           backend.URL,
				CAFile:        backend.CAFile,
				Timeout:       backend.Timeout,
				RetryCount:    backend.RetryCount,
				RetryInterval: backend.RetryInterval,
				Log:           log,
			}
		}

		multiSender, err := callback.NewMultiSender(senderConfigs, log)
		if err != nil {
			log.Error(err, "unable to create drift callback senders")
			os.Exit(1)
		}
		if multiSender != nil {
			callbackSender = multiSender
			log.Info("drift callbacks enabled", "backends", multiSender.Len())
		}
	}

	// Create policy store (uses manager's client which has caching)
	policyStore := policy.NewStore(mgr.GetClient(), log)

	// Set up watch-driven policy watcher - updates store instantly on any policy change
	if err := policy.SetupWatcher(mgr, policyStore, log); err != nil {
		log.Error(err, "unable to set up policy watcher")
		os.Exit(1)
	}
	log.Info("policy watcher configured (watch-driven, instant updates)")

	// Setup signal handling context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start manager in background (runs the policy watcher)
	go func() {
		log.Info("starting controller manager for policy watching")
		if err := mgr.Start(ctx); err != nil {
			log.Error(err, "controller manager failed")
			cancel()
		}
	}()

	// Wait for cache sync before serving webhooks (with timeout)
	syncCtx, syncCancel := context.WithTimeout(ctx, 30*time.Second)
	defer syncCancel()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		log.Error(nil, "cache sync timed out, continuing without watch-driven updates")
	} else {
		log.Info("cache synced, policy store ready")
	}

	// Create and start webhook server
	server := webhook.NewServer(webhook.Config{
		Client:                 mgr.GetClient(),
		Log:                    log,
		Host:                   host,
		Port:                   port,
		CertDir:                certDir,
		HealthProbeBindAddress: healthProbeBindAddress,
		DriftConfig:            driftConfig,
		CallbackSender:         callbackSender,
		PolicyResolver:         policyStore,
	})

	server.Register()

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
