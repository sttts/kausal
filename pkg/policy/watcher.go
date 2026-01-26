package policy

import (
	"context"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kausalityv1alpha1 "github.com/kausality-io/kausality/api/v1alpha1"
)

// Watcher watches Kausality CRDs and keeps the Store updated in realtime.
type Watcher struct {
	client client.Client
	store  *Store
	log    logr.Logger
}

// NewWatcher creates a new policy watcher.
func NewWatcher(c client.Client, store *Store, log logr.Logger) *Watcher {
	return &Watcher{
		client: c,
		store:  store,
		log:    log.WithName("policy-watcher"),
	}
}

// Reconcile is called when any Kausality resource changes.
// It refreshes the entire policy store to keep it in sync.
func (w *Watcher) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	w.log.V(1).Info("policy changed, refreshing store", "name", req.Name)

	if err := w.store.Refresh(ctx); err != nil {
		w.log.Error(err, "failed to refresh policy store")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// SetupWithManager registers the watcher with the controller manager.
func (w *Watcher) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("policy-watcher").
		For(&kausalityv1alpha1.Kausality{}).
		Complete(w)
}

// WatcherReconciler adapts Watcher to the Reconciler interface.
type WatcherReconciler struct {
	*Watcher
}

// Reconcile implements reconcile.Reconciler.
func (r *WatcherReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	return r.Watcher.Reconcile(ctx, req)
}

// SetupWithManager registers the watcher with the controller manager.
func SetupWatcher(mgr ctrl.Manager, store *Store, log logr.Logger) error {
	w := &WatcherReconciler{
		Watcher: NewWatcher(mgr.GetClient(), store, log),
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("policy-watcher").
		For(&kausalityv1alpha1.Kausality{}).
		Complete(w)
}

// InMemoryStore provides a way to manually trigger updates for testing.
func (s *Store) Update(policies []kausalityv1alpha1.Kausality) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies = policies
	s.log.V(1).Info("policies updated", "count", len(policies))
}

// OnChange can be called by the watcher when policies change.
// It's a convenience method that fetches and updates in one call.
func (s *Store) OnChange(ctx context.Context, _ types.NamespacedName) error {
	return s.Refresh(ctx)
}
