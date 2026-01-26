package callback

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	ktesting "github.com/kausality-io/kausality/pkg/testing"
)

func TestTracker_Track(t *testing.T) {
	tracker := NewTracker()

	// First track should return true (new ID)
	assert.True(t, tracker.Track("id1"))

	// Second track of same ID should return false (already tracked)
	assert.False(t, tracker.Track("id1"))

	// Different ID should return true
	assert.True(t, tracker.Track("id2"))
}

func TestTracker_IsTracked(t *testing.T) {
	tracker := NewTracker()

	// Not tracked yet
	assert.False(t, tracker.IsTracked("id1"))

	// Track it
	tracker.Track("id1")

	// Now tracked
	assert.True(t, tracker.IsTracked("id1"))

	// Different ID not tracked
	assert.False(t, tracker.IsTracked("id2"))
}

func TestTracker_Remove(t *testing.T) {
	tracker := NewTracker()

	tracker.Track("id1")
	assert.True(t, tracker.IsTracked("id1"))

	tracker.Remove("id1")
	assert.False(t, tracker.IsTracked("id1"))

	// Track again should work
	assert.True(t, tracker.Track("id1"))
}

func TestTracker_Expiration(t *testing.T) {
	now := time.Now()
	currentTime := now

	tracker := NewTracker(
		WithTTL(5*time.Minute),
		WithNowFunc(func() time.Time { return currentTime }),
	)

	// Track an ID
	assert.True(t, tracker.Track("id1"))
	assert.True(t, tracker.IsTracked("id1"))

	// Advance time but not past TTL
	currentTime = now.Add(3 * time.Minute)
	assert.True(t, tracker.IsTracked("id1"))
	assert.False(t, tracker.Track("id1")) // Still tracked

	// Advance time past TTL
	currentTime = now.Add(6 * time.Minute)
	assert.False(t, tracker.IsTracked("id1"))
	assert.True(t, tracker.Track("id1")) // Can track again
}

func TestTracker_Cleanup(t *testing.T) {
	now := time.Now()
	currentTime := now

	tracker := NewTracker(
		WithTTL(5*time.Minute),
		WithNowFunc(func() time.Time { return currentTime }),
	)

	// Track some IDs
	tracker.Track("id1")
	tracker.Track("id2")
	tracker.Track("id3")
	assert.Equal(t, 3, tracker.Size())

	// Advance time past TTL for id1 and id2
	currentTime = now.Add(6 * time.Minute)

	// Track id3 again to refresh its expiry
	tracker.Track("id3")

	// Cleanup should remove expired entries
	removed := tracker.Cleanup()
	assert.Equal(t, 2, removed) // id1 and id2 expired
	assert.Equal(t, 1, tracker.Size())
	assert.True(t, tracker.IsTracked("id3"))
}

func TestTracker_Size(t *testing.T) {
	tracker := NewTracker()

	assert.Equal(t, 0, tracker.Size())

	tracker.Track("id1")
	assert.Equal(t, 1, tracker.Size())

	tracker.Track("id2")
	assert.Equal(t, 2, tracker.Size())

	tracker.Track("id1") // Duplicate, shouldn't increase size
	assert.Equal(t, 2, tracker.Size())

	tracker.Remove("id1")
	assert.Equal(t, 1, tracker.Size())
}

func TestTracker_WithCustomTTL(t *testing.T) {
	tracker := NewTracker(WithTTL(1 * time.Hour))
	assert.Equal(t, 1*time.Hour, tracker.ttl)
}

func TestTracker_Concurrent(t *testing.T) {
	tracker := NewTracker()
	done := make(chan bool)

	// Run multiple goroutines tracking IDs
	for i := 0; i < 10; i++ {
		go func(n int) {
			for j := 0; j < 100; j++ {
				id := "id" + string(rune('A'+n)) + string(rune('0'+j%10))
				tracker.Track(id)
				tracker.IsTracked(id)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Should not panic or deadlock
	assert.True(t, tracker.Size() > 0)
}

func TestTracker_StartCleanupLoop(t *testing.T) {
	now := time.Now()
	var mu sync.RWMutex
	currentTime := now

	getNow := func() time.Time {
		mu.RLock()
		defer mu.RUnlock()
		return currentTime
	}
	setNow := func(newTime time.Time) {
		mu.Lock()
		defer mu.Unlock()
		currentTime = newTime
	}

	tracker := NewTracker(
		WithTTL(10*time.Millisecond),
		WithNowFunc(getNow),
	)

	tracker.Track("id1")
	assert.Equal(t, 1, tracker.Size())

	// Start cleanup loop
	stop := tracker.StartCleanupLoop(5 * time.Millisecond)
	defer stop()

	// Advance time past TTL
	setNow(now.Add(20 * time.Millisecond))

	// Wait for cleanup loop to clean up
	ktesting.Eventually(t, func() (bool, string) {
		size := tracker.Size()
		if size != 0 {
			return false, fmt.Sprintf("size=%d, waiting for 0", size)
		}
		return true, "cleanup complete"
	}, 1*time.Second, 5*time.Millisecond, "waiting for cleanup loop")
}
