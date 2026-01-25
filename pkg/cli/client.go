package cli

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kausality-io/kausality/pkg/approval"
)

// Client interacts with the Kubernetes API for drift management
type Client struct {
	k8s       client.Client
	applier   *approval.ActionApplier
	namespace string
}

// NewClient creates a new CLI client
func NewClient(k8s client.Client, namespace string) *Client {
	return &Client{
		k8s:       k8s,
		applier:   approval.NewActionApplier(k8s),
		namespace: namespace,
	}
}

// ListDrifts returns all objects with drift annotations in the namespace
func (c *Client) ListDrifts(ctx context.Context, gvk schema.GroupVersionKind) ([]DriftItem, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk)

	opts := []client.ListOption{}
	if c.namespace != "" {
		opts = append(opts, client.InNamespace(c.namespace))
	}

	if err := c.k8s.List(ctx, list, opts...); err != nil {
		return nil, err
	}

	var items []DriftItem
	for _, obj := range list.Items {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			continue
		}

		// Check for pending approvals (indicates drift)
		approvalsStr := annotations[approval.ApprovalsAnnotation]
		if approvalsStr == "" {
			continue
		}

		approvals, err := approval.ParseApprovals(approvalsStr)
		if err != nil {
			continue
		}

		// Each approval entry represents a drift that was approved
		for _, appr := range approvals {
			items = append(items, DriftItem{
				ID:               generateItemID(obj, appr),
				Phase:            "Detected",
				ParentAPIVersion: obj.GetAPIVersion(),
				ParentKind:       obj.GetKind(),
				ParentNamespace:  obj.GetNamespace(),
				ParentName:       obj.GetName(),
				ChildAPIVersion:  appr.APIVersion,
				ChildKind:        appr.Kind,
				ChildNamespace:   obj.GetNamespace(),
				ChildName:        appr.Name,
				User:             "unknown",
				Operation:        "UPDATE",
				DetectedAt:       time.Now(),
			})
		}
	}

	return items, nil
}

func generateItemID(obj unstructured.Unstructured, appr approval.Approval) string {
	return obj.GetNamespace() + "/" + obj.GetName() + "/" + appr.Kind + "/" + appr.Name
}

// ApproveOnce applies a one-time approval for the drift
func (c *Client) ApproveOnce(ctx context.Context, item DriftItem) error {
	return c.applier.ApplyApproval(ctx,
		approval.ObjectRef{
			APIVersion: item.ParentAPIVersion,
			Kind:       item.ParentKind,
			Namespace:  item.ParentNamespace,
			Name:       item.ParentName,
		},
		approval.ChildRef{
			APIVersion: item.ChildAPIVersion,
			Kind:       item.ChildKind,
			Name:       item.ChildName,
		},
		approval.ModeOnce,
	)
}

// ApproveGeneration applies a generation-based approval for the drift
func (c *Client) ApproveGeneration(ctx context.Context, item DriftItem) error {
	return c.applier.ApplyApproval(ctx,
		approval.ObjectRef{
			APIVersion: item.ParentAPIVersion,
			Kind:       item.ParentKind,
			Namespace:  item.ParentNamespace,
			Name:       item.ParentName,
		},
		approval.ChildRef{
			APIVersion: item.ChildAPIVersion,
			Kind:       item.ChildKind,
			Name:       item.ChildName,
		},
		approval.ModeGeneration,
	)
}

// Ignore applies an always-approve for the drift (ignore future drifts)
func (c *Client) Ignore(ctx context.Context, item DriftItem) error {
	return c.applier.ApplyApproval(ctx,
		approval.ObjectRef{
			APIVersion: item.ParentAPIVersion,
			Kind:       item.ParentKind,
			Namespace:  item.ParentNamespace,
			Name:       item.ParentName,
		},
		approval.ChildRef{
			APIVersion: item.ChildAPIVersion,
			Kind:       item.ChildKind,
			Name:       item.ChildName,
		},
		approval.ModeAlways,
	)
}

// Freeze applies a rejection for the drift
func (c *Client) Freeze(ctx context.Context, item DriftItem, reason string) error {
	return c.applier.ApplyRejection(ctx,
		approval.ObjectRef{
			APIVersion: item.ParentAPIVersion,
			Kind:       item.ParentKind,
			Namespace:  item.ParentNamespace,
			Name:       item.ParentName,
		},
		approval.ChildRef{
			APIVersion: item.ChildAPIVersion,
			Kind:       item.ChildKind,
			Name:       item.ChildName,
		},
		reason,
	)
}

// Snooze applies a snooze duration on the parent
func (c *Client) Snooze(ctx context.Context, item DriftItem, duration time.Duration, user, message string) error {
	return c.applier.ApplySnooze(ctx,
		approval.ObjectRef{
			APIVersion: item.ParentAPIVersion,
			Kind:       item.ParentKind,
			Namespace:  item.ParentNamespace,
			Name:       item.ParentName,
		},
		duration,
		user,
		message,
	)
}
