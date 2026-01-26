// Package controller provides controller identification via user hash tracking.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kausality-io/kausality/api/v1alpha1"
)

// Annotation keys - re-exported from api/v1alpha1.
const (
	ControllersAnnotation = v1alpha1.ControllersAnnotation
	UpdatersAnnotation    = v1alpha1.UpdatersAnnotation
	MaxHashes             = v1alpha1.MaxHashes
)

const (
	// asyncUpdateDelay is the delay before async annotation updates.
	// Set to 0 for immediate recording - necessary because status subresource
	// patches to metadata don't persist (Kubernetes only updates .status).
	asyncUpdateDelay = 0
)

// Tracker tracks controller identity via user hash annotations.
type Tracker struct {
	client client.Client
	log    logr.Logger

	// pending tracks async updates to batch
	pending   map[string]string // objectKey -> hash to add
	pendingMu sync.Mutex
}

// NewTracker creates a new controller Tracker.
func NewTracker(c client.Client, log logr.Logger) *Tracker {
	return &Tracker{
		client:  c,
		log:     log.WithName("controller-tracker"),
		pending: make(map[string]string),
	}
}

// UserIdentifier returns the user identifier to use for hashing.
// Uses username if non-empty, otherwise falls back to UID.
func UserIdentifier(username, uid string) string {
	if username != "" {
		return username
	}
	return uid
}

// HashUsername creates a 5-character base36 hash of a username (or UID).
func HashUsername(username string) string {
	h := sha256.Sum256([]byte(username))
	// Use first 4 bytes as uint32, convert to base36
	n := binary.BigEndian.Uint32(h[:4])
	s := strconv.FormatUint(uint64(n), 36)
	// Pad to 5 chars if needed
	for len(s) < 5 {
		s = "0" + s
	}
	return s[:5]
}

// RecordUpdater adds a user hash to the child's updaters annotation.
// This is called synchronously and returns the patch data.
func RecordUpdater(obj client.Object, username string) map[string]string {
	hash := HashUsername(username)
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// Get existing hashes
	existing := annotations[UpdatersAnnotation]
	hashes := ParseHashes(existing)

	// Add new hash if not already present
	if !ContainsHash(hashes, hash) {
		hashes = append(hashes, hash)
		// Limit to MaxHashes (keep most recent)
		if len(hashes) > MaxHashes {
			hashes = hashes[len(hashes)-MaxHashes:]
		}
	}

	annotations[UpdatersAnnotation] = strings.Join(hashes, ",")
	return annotations
}

// RecordControllerAsync schedules an async update to add the user hash
// to the parent's controllers annotation.
func (t *Tracker) RecordControllerAsync(ctx context.Context, obj client.Object, username string) {
	hash := HashUsername(username)
	key := objectKey(obj)

	// Check if hash is already in annotation
	annotations := obj.GetAnnotations()
	if annotations != nil {
		existing := annotations[ControllersAnnotation]
		if ContainsHash(ParseHashes(existing), hash) {
			return // Already recorded
		}
	}

	t.pendingMu.Lock()
	_, alreadyPending := t.pending[key]
	t.pending[key] = hash
	t.pendingMu.Unlock()

	if !alreadyPending {
		if asyncUpdateDelay == 0 {
			// Synchronous recording - necessary because status subresource
			// patches to metadata don't persist, so we must update via direct API call
			// before the next admission request arrives.
			t.flushAfterDelay(ctx, obj, 0)
		} else {
			// Schedule the update with delay
			go t.flushAfterDelay(ctx, obj, asyncUpdateDelay)
		}
	}
}

// flushAfterDelay waits and then updates the annotation.
func (t *Tracker) flushAfterDelay(ctx context.Context, obj client.Object, delay time.Duration) {
	time.Sleep(delay)

	key := objectKey(obj)
	t.pendingMu.Lock()
	hash, ok := t.pending[key]
	delete(t.pending, key)
	t.pendingMu.Unlock()

	if !ok {
		return
	}

	log := t.log.WithValues(
		"kind", objectTypeName(obj),
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
		"hash", hash,
	)

	// DeepCopy once, reuse in retry loop
	current := obj.DeepCopyObject().(client.Object)

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := t.client.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}

		// Get existing hashes
		annotations := current.GetAnnotations()
		hashes := ParseHashes(annotations[ControllersAnnotation])

		// Check if already present
		if ContainsHash(hashes, hash) {
			return nil
		}

		// Add new hash
		hashes = append(hashes, hash)
		if len(hashes) > MaxHashes {
			hashes = hashes[len(hashes)-MaxHashes:]
		}

		// Initialize map only before writing
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[ControllersAnnotation] = strings.Join(hashes, ",")
		current.SetAnnotations(annotations)

		return t.client.Update(ctx, current)
	})

	if err != nil {
		log.Error(err, "failed to update controllers annotation")
	} else {
		log.V(1).Info("recorded controller hash")
	}
}

// ParseHashes splits a comma-separated hash string.
func ParseHashes(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// ContainsHash checks if a hash is in the list.
func ContainsHash(hashes []string, hash string) bool {
	for _, h := range hashes {
		if h == hash {
			return true
		}
	}
	return false
}

// Intersect returns hashes present in both lists.
func Intersect(a, b []string) []string {
	set := make(map[string]struct{})
	for _, h := range a {
		set[h] = struct{}{}
	}
	var result []string
	for _, h := range b {
		if _, ok := set[h]; ok {
			result = append(result, h)
		}
	}
	return result
}

// Phase annotation and values - re-exported from api/v1alpha1.
const (
	PhaseAnnotation        = v1alpha1.PhaseAnnotation
	PhaseValueInitializing = v1alpha1.PhaseValueInitializing
	PhaseValueInitialized  = v1alpha1.PhaseValueInitialized
)

// RecordPhaseAsync schedules an async update to set the phase annotation.
// Only records when transitioning to initialized (never downgrades).
func (t *Tracker) RecordPhaseAsync(ctx context.Context, obj client.Object, phase string) {
	// Skip if deleting (derived from metadata, not stored)
	if obj.GetDeletionTimestamp() != nil {
		return
	}

	// Skip if already initialized (don't downgrade)
	annotations := obj.GetAnnotations()
	if annotations != nil && annotations[PhaseAnnotation] == PhaseValueInitialized {
		return
	}

	// Skip if setting to same value
	if annotations != nil && annotations[PhaseAnnotation] == phase {
		return
	}

	key := objectKey(obj) + "/phase"

	t.pendingMu.Lock()
	_, alreadyPending := t.pending[key]
	t.pending[key] = phase
	t.pendingMu.Unlock()

	if !alreadyPending {
		go t.flushPhaseAfterDelay(ctx, obj, asyncUpdateDelay)
	}
}

// flushPhaseAfterDelay waits and then updates the phase annotation.
func (t *Tracker) flushPhaseAfterDelay(ctx context.Context, obj client.Object, delay time.Duration) {
	time.Sleep(delay)

	key := objectKey(obj) + "/phase"
	t.pendingMu.Lock()
	phase, ok := t.pending[key]
	delete(t.pending, key)
	t.pendingMu.Unlock()

	if !ok {
		return
	}

	log := t.log.WithValues(
		"kind", objectTypeName(obj),
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
		"phase", phase,
	)

	// DeepCopy once, reuse in retry loop
	current := obj.DeepCopyObject().(client.Object)

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := t.client.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}

		// Check if already initialized (don't downgrade)
		annotations := current.GetAnnotations()
		if annotations != nil && annotations[PhaseAnnotation] == PhaseValueInitialized {
			return nil
		}

		// Check if already set to this value
		if annotations != nil && annotations[PhaseAnnotation] == phase {
			return nil
		}

		// Initialize map only before writing
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[PhaseAnnotation] = phase
		current.SetAnnotations(annotations)

		return t.client.Update(ctx, current)
	})

	if err != nil {
		log.Error(err, "failed to update phase annotation")
	} else {
		log.V(1).Info("recorded phase")
	}
}

// objectKey returns a string key for an object.
func objectKey(obj client.Object) string {
	return objectTypeName(obj) + "/" + obj.GetNamespace() + "/" + obj.GetName()
}

// objectTypeName returns a readable type name for an object.
// Uses GVK if available (unstructured), otherwise falls back to Go type name.
func objectTypeName(obj client.Object) string {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Kind != "" {
		return gvk.Kind
	}
	return reflect.TypeOf(obj).Elem().Name()
}
