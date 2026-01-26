# Deployment Modes

The core logic is implemented as a **Go library** that can be consumed in two ways.

## Architecture Overview

When deployed as a webhook, Kausality consists of two components:

| Component | Binary | ServiceAccount | Purpose |
|-----------|--------|----------------|---------|
| **Webhook** | `kausality-webhook` | `kausality-webhook` | Intercepts mutations and detects drift |
| **Controller** | `kausality-controller` | `kausality-controller` | Watches Kausality CRDs, reconciles webhook configuration |

**RBAC:**

| ClusterRole | Bound To | Purpose |
|-------------|----------|---------|
| `kausality-webhook` | webhook | Read Kausality policies and namespaces for mode resolution |
| `kausality-webhook-resources` | webhook | Aggregated access to tracked resources (auto-populated) |
| `kausality-controller` | controller | Manage CRDs, webhook config, and per-policy ClusterRoles |

The controller generates per-policy ClusterRoles (e.g., `kausality-policy-apps-policy`) with the aggregation label `kausality.io/aggregate-to-webhook-resources: "true"`. Kubernetes automatically aggregates these into `kausality-webhook-resources`, giving the webhook access to the resources defined in policies.

## Library Import (Generic Control Plane)

```go
import "github.com/kausality-io/kausality/pkg/admission"

// In apiserver setup
admissionHandler := admission.NewHandler(admission.Config{
    Client:         client,
    Log:            logger,
    DriftConfig:    driftConfig,    // optional, defaults to log mode
    CallbackSender: callbackSender, // optional, for drift notifications
})

// Register as admission plugin
server.RegisterAdmission(admissionHandler)
```

- Embedded directly in custom apiserver (k8s.io/apiserver)
- No network latency, no webhook overhead
- Resource targeting is handled by which admission plugins are registered for which resources

**Working Example:** See [`cmd/example-generic-control-plane/`](../../cmd/example-generic-control-plane/) for a complete implementation with embedded etcd and custom API types (Widget, WidgetSet).

## Webhook Server (Stock Kubernetes)

```go
import "github.com/kausality-io/kausality/pkg/webhook"

// Standalone webhook server
server := webhook.NewServer(webhook.Config{
    Client:         client,
    Log:            logger,
    CertDir:        "/etc/webhook/certs",
    DriftConfig:    driftConfig,    // optional
    CallbackSender: callbackSender, // optional
})
server.Register()
server.Start(ctx)
```

- Deployed as separate service
- Configured via ValidatingWebhookConfiguration / MutatingWebhookConfiguration
- Helm chart handles webhook registration
- ValidatingAdmissionPolicy (CEL) for simple fast-path checks:
  - `object.metadata.generation == object.status.observedGeneration` → drift candidate
  - `has(object.metadata.deletionTimestamp)` → deletion phase

## Resource Targeting

Which resources are subject to drift detection is **deployment configuration**, not core logic.

### Configuration Model

```yaml
# For webhook: part of WebhookConfiguration
# For library: passed to admission.Config
resourceRules:
  # Include by API group
  - apiGroups: ["apps"]
    resources: ["*"]

  # Include specific resources
  - apiGroups: ["example.com"]
    resources: ["ekscluster", "nodepools"]

  # Exclude specific resources
  - apiGroups: [""]
    resources: ["configmaps", "secrets"]
    exclude: true
```

### Webhook Configuration (CRD-based)

Resource targeting is configured via Kausality CRDs. The controller watches these CRDs and dynamically updates the MutatingWebhookConfiguration.

```yaml
apiVersion: kausality.io/v1alpha1
kind: Kausality
metadata:
  name: apps-policy
spec:
  resources:
    - apiGroups: ["apps"]
      resources: ["deployments", "replicasets"]
  mode: log
```

The controller reconciles this into webhook rules:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: kausality
webhooks:
- name: mutating.webhook.kausality.io
  rules:
  - apiGroups: ["apps"]
    apiVersions: ["*"]
    resources: ["deployments", "replicasets"]
    operations: ["CREATE", "UPDATE", "DELETE"]
  - apiGroups: ["apps"]
    apiVersions: ["*"]
    resources: ["deployments/status", "replicasets/status"]
    operations: ["UPDATE"]  # For controller hash tracking
```

The controller also generates per-policy ClusterRoles for RBAC:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kausality-policy-apps-policy
  labels:
    kausality.io/aggregate-to-webhook-resources: "true"
rules:
- apiGroups: ["apps"]
  resources: ["deployments", "replicasets"]
  verbs: ["get", "list", "watch", "patch"]
```

### Library Configuration (Generic Control Plane)

For generic control plane, resource targeting is typically hard-coded or loaded from config:

```go
// The library doesn't filter — it processes whatever requests it receives
// Resource targeting is done at the apiserver level (which resources invoke admission)
admission.NewHandler(admission.Config{
    Client:      client,
    Log:         logger,
    DriftConfig: driftConfig,
})
```

In a generic control plane, resource targeting is handled by which admission plugins are registered for which resources — not by the admission logic itself.

### Design Note

Resource targeting is **deployment configuration**, not core library logic:
- **Webhook mode**: Helm chart generates WebhookConfiguration with rules
- **Library mode**: Apiserver registration determines which resources invoke admission

The core admission handler assumes it should process every request it receives. Filtering is external.
