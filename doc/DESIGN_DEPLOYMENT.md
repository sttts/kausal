# Deployment Modes

The core logic is implemented as a **Go library** that can be consumed in two ways.

## Library Import (Generic Control Plane)

```go
import "github.com/sttts/kausality/pkg/admission"

// In apiserver setup
admissionHandler := admission.NewHandler(admission.Config{
    Client:        client,
    PolicyLister:  policyInformer.Lister(),
    // ...
})

// Register as admission plugin
server.RegisterAdmission(admissionHandler)
```

- Embedded directly in custom apiserver (k8s.io/apiserver)
- No network latency, no webhook overhead
- ApprovalPolicy CRD served by same apiserver
- Resource targeting is handled by which admission plugins are registered for which resources

## Webhook Server (Stock Kubernetes)

```go
import "github.com/sttts/kausality/pkg/webhook"

// Standalone webhook server
server := webhook.NewServer(webhook.Config{
    Client:       client,
    PolicyLister: policyInformer.Lister(),
    CertDir:      "/etc/webhook/certs",
    // ...
})
server.Run()
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

### Webhook Configuration (Helm)

For stock Kubernetes, the Helm chart generates WebhookConfiguration:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: kausality-drift-detection
webhooks:
- name: drift.kausality.io
  rules:
  - apiGroups: ["apps", "example.com"]
    apiVersions: ["*"]
    resources: ["deployments", "ekscluster", "nodepools"]
    operations: ["CREATE", "UPDATE", "DELETE"]
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: NotIn
      values: ["kube-system", "kube-public"]
```

Helm values:
```yaml
resourceRules:
  include:
  - apiGroups: ["apps"]
    resources: ["*"]
  - apiGroups: ["example.com"]
    resources: ["ekscluster", "nodepools"]
  exclude:
  - apiGroups: [""]
    resources: ["configmaps", "secrets"]

excludeNamespaces:
  - kube-system
  - kube-public
```

### Library Configuration (Generic Control Plane)

For generic control plane, resource targeting is typically hard-coded or loaded from config:

```go
// The library doesn't filter — it processes whatever requests it receives
// Resource targeting is done at the apiserver level (which resources invoke admission)
admission.NewHandler(admission.Config{
    Client:       client,
    PolicyLister: policyInformer.Lister(),
})
```

In a generic control plane, resource targeting is handled by which admission plugins are registered for which resources — not by the admission logic itself.

### Design Note

Resource targeting is **deployment configuration**, not core library logic:
- **Webhook mode**: Helm chart generates WebhookConfiguration with rules
- **Library mode**: Apiserver registration determines which resources invoke admission

The core admission handler assumes it should process every request it receives. Filtering is external.
