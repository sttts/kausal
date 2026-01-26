package callback

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kausality-io/kausality/pkg/callback/v1alpha1"
	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

func TestNewMultiSender_EmptyConfigs(t *testing.T) {
	ms, err := NewMultiSender(nil, logr.Discard())
	require.NoError(t, err)
	assert.Nil(t, ms)

	ms, err = NewMultiSender([]SenderConfig{}, logr.Discard())
	require.NoError(t, err)
	assert.Nil(t, ms)
}

func TestNewMultiSender_SkipsEmptyURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := v1alpha1.DriftReportResponse{Acknowledged: true}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	ms, err := NewMultiSender([]SenderConfig{
		{URL: ""},
		{URL: server.URL},
		{URL: ""},
	}, logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, ms)
	assert.Equal(t, 1, ms.Len())
}

func TestNewMultiSender_AllEmptyURLs(t *testing.T) {
	ms, err := NewMultiSender([]SenderConfig{
		{URL: ""},
		{URL: ""},
	}, logr.Discard())
	require.NoError(t, err)
	assert.Nil(t, ms)
}

func TestMultiSender_SendAsync_FansOut(t *testing.T) {
	var wg sync.WaitGroup
	var counts [3]atomic.Int32

	// Create 3 test servers
	servers := make([]*httptest.Server, 3)
	for i := 0; i < 3; i++ {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[idx].Add(1)
			response := v1alpha1.DriftReportResponse{Acknowledged: true}
			_ = json.NewEncoder(w).Encode(response)
		}))
		defer servers[i].Close()
	}

	configs := make([]SenderConfig, 3)
	for i, s := range servers {
		configs[i] = SenderConfig{URL: s.URL, Log: logr.Discard()}
	}

	ms, err := NewMultiSender(configs, logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, ms)
	assert.Equal(t, 3, ms.Len())

	report := &v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "fan-out-test",
			Phase: v1alpha1.DriftReportPhaseDetected,
		},
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		ms.SendAsync(context.Background(), report)
	}()

	// Wait for async sends to complete
	wg.Wait()

	// Poll until all backends have received the report
	require.Eventually(t, func() bool {
		for i := 0; i < 3; i++ {
			if counts[i].Load() != 1 {
				return false
			}
		}
		return true
	}, time.Second, 10*time.Millisecond, "all backends should receive 1 report")
}

func TestMultiSender_IsEnabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := v1alpha1.DriftReportResponse{Acknowledged: true}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	ms, err := NewMultiSender([]SenderConfig{{URL: server.URL}}, logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, ms)
	assert.True(t, ms.IsEnabled())
}

func TestMultiSender_MarkResolved(t *testing.T) {
	var counts [2]atomic.Int32

	// Create 2 test servers
	servers := make([]*httptest.Server, 2)
	for i := 0; i < 2; i++ {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[idx].Add(1)
			response := v1alpha1.DriftReportResponse{Acknowledged: true}
			_ = json.NewEncoder(w).Encode(response)
		}))
		defer servers[i].Close()
	}

	configs := make([]SenderConfig, 2)
	for i, s := range servers {
		configs[i] = SenderConfig{URL: s.URL, Log: logr.Discard()}
	}

	ms, err := NewMultiSender(configs, logr.Discard())
	require.NoError(t, err)

	report := &v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "mark-resolved-multi",
			Phase: v1alpha1.DriftReportPhaseDetected,
		},
	}

	// Send first report
	ms.SendAsync(context.Background(), report)
	ktesting.Eventually(t, func() (bool, string) {
		c0, c1 := counts[0].Load(), counts[1].Load()
		if c0 != 1 || c1 != 1 {
			return false, fmt.Sprintf("counts=[%d,%d], want [1,1]", c0, c1)
		}
		return true, "both received"
	}, 5*time.Second, 10*time.Millisecond, "waiting for first send")

	// Send again - should be deduplicated on both (counts stay at 1)
	ms.SendAsync(context.Background(), report)
	// Brief wait then verify no change (deduplication)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), counts[0].Load())
	assert.Equal(t, int32(1), counts[1].Load())

	// Mark as resolved
	ms.MarkResolved("mark-resolved-multi")

	// Now it can be sent again
	ms.SendAsync(context.Background(), report)
	ktesting.Eventually(t, func() (bool, string) {
		c0, c1 := counts[0].Load(), counts[1].Load()
		if c0 != 2 || c1 != 2 {
			return false, fmt.Sprintf("counts=[%d,%d], want [2,2]", c0, c1)
		}
		return true, "both received again"
	}, 5*time.Second, 10*time.Millisecond, "waiting for resend")
}

func TestMultiSender_IndependentDeduplication(t *testing.T) {
	// Test that each sender has independent deduplication tracking
	var counts [2]atomic.Int32

	servers := make([]*httptest.Server, 2)
	for i := 0; i < 2; i++ {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[idx].Add(1)
			response := v1alpha1.DriftReportResponse{Acknowledged: true}
			_ = json.NewEncoder(w).Encode(response)
		}))
		defer servers[i].Close()
	}

	configs := make([]SenderConfig, 2)
	for i, s := range servers {
		configs[i] = SenderConfig{URL: s.URL, Log: logr.Discard()}
	}

	ms, err := NewMultiSender(configs, logr.Discard())
	require.NoError(t, err)

	report := &v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "independent-dedup",
			Phase: v1alpha1.DriftReportPhaseDetected,
		},
	}

	// Send once - both should receive
	ms.SendAsync(context.Background(), report)
	ktesting.Eventually(t, func() (bool, string) {
		c0, c1 := counts[0].Load(), counts[1].Load()
		if c0 != 1 || c1 != 1 {
			return false, fmt.Sprintf("counts=[%d,%d], want [1,1]", c0, c1)
		}
		return true, "both received"
	}, 5*time.Second, 10*time.Millisecond, "waiting for first send")

	// Send again - neither should receive (independent dedup)
	ms.SendAsync(context.Background(), report)
	// Brief wait then verify no change (deduplication)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), counts[0].Load())
	assert.Equal(t, int32(1), counts[1].Load())
}

func TestMultiSender_StartCleanup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := v1alpha1.DriftReportResponse{Acknowledged: true}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	ms, err := NewMultiSender([]SenderConfig{{URL: server.URL}}, logr.Discard())
	require.NoError(t, err)

	// Start cleanup
	stop := ms.StartCleanup(10 * time.Millisecond)

	// Let it run for a bit
	time.Sleep(50 * time.Millisecond)

	// Stop cleanup
	stop()

	// No panic or error means success
}

func TestMultiSender_ReportWithNewObject(t *testing.T) {
	var receivedReports []*v1alpha1.DriftReport
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		report := &v1alpha1.DriftReport{}
		_ = json.Unmarshal(body, report)

		mu.Lock()
		receivedReports = append(receivedReports, report)
		mu.Unlock()

		response := v1alpha1.DriftReportResponse{Acknowledged: true}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	ms, err := NewMultiSender([]SenderConfig{{URL: server.URL}}, logr.Discard())
	require.NoError(t, err)

	report := &v1alpha1.DriftReport{
		Spec: v1alpha1.DriftReportSpec{
			ID:    "with-new-object",
			Phase: v1alpha1.DriftReportPhaseDetected,
			Parent: v1alpha1.ObjectReference{
				APIVersion: "example.com/v1",
				Kind:       "Parent",
				Name:       "test-parent",
			},
			Child: v1alpha1.ObjectReference{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       "test-child",
			},
		},
	}

	ms.SendAsync(context.Background(), report)
	ktesting.Eventually(t, func() (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		if len(receivedReports) != 1 {
			return false, fmt.Sprintf("received %d reports, want 1", len(receivedReports))
		}
		return true, "report received"
	}, 5*time.Second, 10*time.Millisecond, "waiting for report")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "with-new-object", receivedReports[0].Spec.ID)
}

// Ensure interface compliance at compile time
func TestMultiSender_ImplementsReportSender(t *testing.T) {
	var _ ReportSender = (*MultiSender)(nil)
}
