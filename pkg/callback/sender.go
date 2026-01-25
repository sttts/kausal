package callback

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kausality-io/kausality/pkg/callback/v1alpha1"
)

// ReportSender sends drift reports to backend endpoints.
type ReportSender interface {
	SendAsync(ctx context.Context, report *v1alpha1.DriftReport)
	IsEnabled() bool
	MarkResolved(id string)
	StartCleanup(interval time.Duration) func()
}

// SenderConfig configures the Sender.
type SenderConfig struct {
	// URL is the webhook endpoint URL.
	URL string
	// CAFile is the path to the CA certificate file for TLS verification.
	// If empty, system CA pool is used.
	CAFile string
	// Timeout is the request timeout. Default is 10 seconds.
	Timeout time.Duration
	// RetryCount is the number of retries on failure. Default is 3.
	RetryCount int
	// RetryInterval is the interval between retries. Default is 1 second.
	RetryInterval time.Duration
	// Log is the logger. If nil, a noop logger is used.
	Log logr.Logger
}

// Sender sends DriftReports to webhook endpoints.
type Sender struct {
	config  SenderConfig
	client  *http.Client
	tracker *Tracker
	log     logr.Logger
}

// NewSender creates a new Sender with the given configuration.
func NewSender(cfg SenderConfig) (*Sender, error) {
	// Apply defaults
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.RetryCount == 0 {
		cfg.RetryCount = 3
	}
	if cfg.RetryInterval == 0 {
		cfg.RetryInterval = 1 * time.Second
	}

	// Create TLS config
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if cfg.CAFile != "" {
		caCert, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caCertPool
	}

	client := &http.Client{
		Timeout: cfg.Timeout,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	log := cfg.Log
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	return &Sender{
		config:  cfg,
		client:  client,
		tracker: NewTracker(),
		log:     log.WithName("drift-callback"),
	}, nil
}

// Send sends a DriftReport to the configured webhook endpoint.
// This is a blocking call; use SendAsync for non-blocking behavior.
func (s *Sender) Send(ctx context.Context, report *v1alpha1.DriftReport) error {
	// Set TypeMeta
	report.TypeMeta = metav1.TypeMeta{
		APIVersion: v1alpha1.GroupName + "/" + v1alpha1.Version,
		Kind:       "DriftReport",
	}

	// Check for deduplication (only for Detected phase)
	if report.Spec.Phase == v1alpha1.DriftReportPhaseDetected {
		if !s.tracker.Track(report.Spec.ID) {
			s.log.V(1).Info("skipping duplicate drift report", "id", report.Spec.ID)
			return nil
		}
	}

	// Marshal report
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("failed to marshal drift report: %w", err)
	}

	// Send with retry
	var lastErr error
	for attempt := 0; attempt <= s.config.RetryCount; attempt++ {
		if attempt > 0 {
			s.log.V(1).Info("retrying drift report",
				"attempt", attempt,
				"id", report.Spec.ID,
				"lastError", lastErr,
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.config.RetryInterval):
			}
		}

		lastErr = s.doSend(ctx, body, report.Spec.ID)
		if lastErr == nil {
			return nil
		}
	}

	s.log.Error(lastErr, "failed to send drift report after retries",
		"id", report.Spec.ID,
		"retries", s.config.RetryCount,
	)
	return lastErr
}

// doSend performs a single send attempt.
func (s *Sender) doSend(ctx context.Context, body []byte, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// Check status code
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response
	var response v1alpha1.DriftReportResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		// Log but don't fail if response can't be parsed
		s.log.V(1).Info("could not parse webhook response",
			"id", id,
			"body", string(respBody),
		)
		return nil
	}

	if !response.Acknowledged {
		return fmt.Errorf("webhook did not acknowledge: %s", response.Error)
	}

	s.log.Info("drift report sent successfully", "id", id)
	return nil
}

// SendAsync sends a DriftReport asynchronously.
// The report is sent in a goroutine and any errors are logged but not returned.
// Uses a background context since the original request context may be canceled.
func (s *Sender) SendAsync(_ context.Context, report *v1alpha1.DriftReport) {
	go func() {
		// Use background context since the admission request context will be canceled
		// after the response is sent, but we still want to complete the HTTP request.
		if err := s.Send(context.Background(), report); err != nil {
			s.log.Error(err, "async drift report send failed", "id", report.Spec.ID)
		}
	}()
}

// MarkResolved marks a drift as resolved and removes it from the tracker.
// This allows the same drift to be tracked again if it recurs.
func (s *Sender) MarkResolved(id string) {
	s.tracker.Remove(id)
}

// StartCleanup starts a background cleanup loop for the tracker.
// Returns a stop function to cancel the loop.
func (s *Sender) StartCleanup(interval time.Duration) func() {
	return s.tracker.StartCleanupLoop(interval)
}

// IsEnabled returns true if the sender is configured with a URL.
func (s *Sender) IsEnabled() bool {
	return s.config.URL != ""
}
