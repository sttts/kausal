package callback

import (
	"context"
	"time"

	"github.com/go-logr/logr"

	"github.com/kausality-io/kausality/pkg/callback/v1alpha1"
)

// MultiSender wraps multiple Sender instances and fans out reports to all of them.
// Each sender has independent deduplication tracking.
type MultiSender struct {
	senders []*Sender
	log     logr.Logger
}

// NewMultiSender creates a new MultiSender from a list of SenderConfig.
// Returns nil if configs is empty.
func NewMultiSender(configs []SenderConfig, log logr.Logger) (*MultiSender, error) {
	if len(configs) == 0 {
		return nil, nil
	}

	senders := make([]*Sender, 0, len(configs))
	for _, cfg := range configs {
		// Skip empty URLs
		if cfg.URL == "" {
			continue
		}

		// Ensure each sender has a logger
		if cfg.Log.GetSink() == nil {
			cfg.Log = log
		}

		sender, err := NewSender(cfg)
		if err != nil {
			return nil, err
		}
		senders = append(senders, sender)
	}

	if len(senders) == 0 {
		return nil, nil
	}

	return &MultiSender{
		senders: senders,
		log:     log.WithName("multi-sender"),
	}, nil
}

// SendAsync sends a DriftReport to all configured backends in parallel.
// Each backend has independent deduplication tracking.
func (m *MultiSender) SendAsync(ctx context.Context, report *v1alpha1.DriftReport) {
	for _, sender := range m.senders {
		sender.SendAsync(ctx, report)
	}
}

// IsEnabled returns true if at least one sender is configured.
func (m *MultiSender) IsEnabled() bool {
	return len(m.senders) > 0
}

// MarkResolved marks a drift as resolved on all senders.
func (m *MultiSender) MarkResolved(id string) {
	for _, sender := range m.senders {
		sender.MarkResolved(id)
	}
}

// StartCleanup starts cleanup loops on all senders.
// Returns a stop function that stops all cleanup loops.
func (m *MultiSender) StartCleanup(interval time.Duration) func() {
	stopFuncs := make([]func(), 0, len(m.senders))
	for _, sender := range m.senders {
		stopFuncs = append(stopFuncs, sender.StartCleanup(interval))
	}
	return func() {
		for _, stop := range stopFuncs {
			stop()
		}
	}
}

// Len returns the number of configured senders.
func (m *MultiSender) Len() int {
	return len(m.senders)
}

// Ensure Sender and MultiSender implement ReportSender.
var (
	_ ReportSender = (*Sender)(nil)
	_ ReportSender = (*MultiSender)(nil)
)
