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
)

const (
	// ControllersAnnotation stores hashes of users who update parent status.
	ControllersAnnotation = "kausality.io/controllers"

	// UpdatersAnnotation stores hashes of users who update child spec.
	UpdatersAnnotation = "kausality.io/updaters"

	// MaxHashes is the maximum number of hashes to store in annotations.
	MaxHashes = 5

	// asyncUpdateDelay is the delay before async annotation updates.
	asyncUpdateDelay = 5 * time.Second
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
		// Schedule the update
		go t.flushAfterDelay(ctx, obj, asyncUpdateDelay)
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

// IdentifyController determines if the current user is the controller.
// Returns (isController, canDetermine).
// If canDetermine is false, we can't reliably identify the controller.
func IdentifyController(child, parent client.Object, username string) (isController bool, canDetermine bool) {
	userHash := HashUsername(username)

	// Get child's updaters
	childAnnotations := child.GetAnnotations()
	var childUpdaters []string
	if childAnnotations != nil {
		childUpdaters = ParseHashes(childAnnotations[UpdatersAnnotation])
	}

	// Get parent's controllers
	parentAnnotations := parent.GetAnnotations()
	var parentControllers []string
	if parentAnnotations != nil {
		parentControllers = ParseHashes(parentAnnotations[ControllersAnnotation])
	}

	// Case 1: Single updater on child - that's the controller
	if len(childUpdaters) == 1 {
		controller := childUpdaters[0]
		return userHash == controller, true
	}

	// Case 2: Multiple updaters + parent has controllers - use intersection
	if len(childUpdaters) > 1 && len(parentControllers) > 0 {
		intersection := Intersect(childUpdaters, parentControllers)
		if len(intersection) > 0 {
			return ContainsHash(intersection, userHash), true
		}
	}

	// Case 3: Can't determine (multiple updaters, no parent controllers)
	return false, false
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
