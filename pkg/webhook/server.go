// Package webhook provides a standalone webhook server for drift detection and tracing.
package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/kausality-io/kausality/pkg/admission"
	"github.com/kausality-io/kausality/pkg/callback"
	"github.com/kausality-io/kausality/pkg/config"
)

// Config configures the webhook server.
type Config struct {
	// Client is the Kubernetes client for resolving parent objects.
	Client client.Client
	// Log is the logger for the server.
	Log logr.Logger
	// Host is the address to bind to. Defaults to "" (all interfaces).
	Host string
	// Port is the port to listen on. Defaults to 9443.
	Port int
	// CertDir is the directory containing tls.crt and tls.key files.
	CertDir string
	// CertName is the name of the TLS certificate file. Defaults to "tls.crt".
	CertName string
	// KeyName is the name of the TLS key file. Defaults to "tls.key".
	KeyName string
	// HealthProbeBindAddress is the address for health probes. Defaults to ":8081".
	HealthProbeBindAddress string
	// DriftConfig provides per-resource drift detection configuration.
	// If nil, defaults to log mode for all resources.
	DriftConfig *config.Config
	// CallbackSender sends drift reports to webhook endpoints.
	// If nil, drift callbacks are disabled.
	CallbackSender callback.ReportSender
}

// Server is a standalone webhook server for drift detection.
type Server struct {
	config        Config
	webhookServer webhook.Server
	healthServer  *http.Server
	log           logr.Logger
}

// NewServer creates a new webhook Server.
func NewServer(cfg Config) *Server {
	// Apply defaults
	if cfg.Port == 0 {
		cfg.Port = 9443
	}
	if cfg.CertName == "" {
		cfg.CertName = "tls.crt"
	}
	if cfg.KeyName == "" {
		cfg.KeyName = "tls.key"
	}
	if cfg.HealthProbeBindAddress == "" {
		cfg.HealthProbeBindAddress = ":8081"
	}

	log := cfg.Log.WithName("webhook-server")

	// Create webhook server options
	opts := webhook.Options{
		Host:     cfg.Host,
		Port:     cfg.Port,
		CertDir:  cfg.CertDir,
		CertName: cfg.CertName,
		KeyName:  cfg.KeyName,
	}

	return &Server{
		config:        cfg,
		webhookServer: webhook.NewServer(opts),
		log:           log,
	}
}

// Register registers the admission handler with the webhook server.
func (s *Server) Register() {
	handler := admission.NewHandler(admission.Config{
		Client:         s.config.Client,
		Log:            s.log,
		DriftConfig:    s.config.DriftConfig,
		CallbackSender: s.config.CallbackSender,
	})

	s.webhookServer.Register("/mutate", &webhook.Admission{Handler: handler})
	s.log.Info("registered kausality webhook", "path", "/mutate")
}

// Start starts the webhook server and health server.
func (s *Server) Start(ctx context.Context) error {
	// Start health server
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	healthMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	s.healthServer = &http.Server{
		Addr:    s.config.HealthProbeBindAddress,
		Handler: healthMux,
	}

	// Start health server in background
	go func() {
		s.log.Info("starting health server", "addr", s.config.HealthProbeBindAddress)
		if err := s.healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error(err, "health server failed")
		}
	}()

	// Start webhook server
	s.log.Info("starting webhook server", "host", s.config.Host, "port", s.config.Port)
	return s.webhookServer.Start(ctx)
}

// Shutdown gracefully shuts down the servers.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.healthServer != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := s.healthServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("health server shutdown failed: %w", err)
		}
	}
	return nil
}

// GetHealthzHandler returns a healthz checker for use with controller-runtime manager.
func GetHealthzHandler() healthz.Checker {
	return healthz.Ping
}

// GetReadyzHandler returns a readyz checker for use with controller-runtime manager.
func GetReadyzHandler() healthz.Checker {
	return healthz.Ping
}

// TLSConfig returns the TLS configuration for the webhook server.
// This can be used for testing or custom configurations.
func TLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS key pair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
