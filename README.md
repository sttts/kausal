<p align="center">
  <img src="logo.png" alt="Kausality Logo" width="200" />
</p>

<h1 align="center">Kausality</h1>

<p align="center">
  <strong style="color: red;">&#9888; EXPERIMENTAL &#9888;</strong>
</p>

<table align="center">
<tr>
<td>
<strong>This project is highly experimental and under active development.</strong><br>
APIs, behavior, and configuration may change without notice.<br>
<strong>Not recommended for production use.</strong>
</td>
</tr>
</table>

<p align="center">
  <em>"Every mutation needs a cause."</em>
</p>

<p align="center">
  <strong>Causal traceability for Kubernetes resource mutations</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Status-Experimental-orange.svg" alt="Experimental">
  <a href="https://github.com/kausality-io/kausality/actions/workflows/ci.yaml"><img src="https://github.com/kausality-io/kausality/actions/workflows/ci.yaml/badge.svg" alt="CI"></a>
  <a href="https://goreportcard.com/report/github.com/kausality-io/kausality"><img src="https://goreportcard.com/badge/github.com/kausality-io/kausality" alt="Go Report Card"></a>
  <a href="https://github.com/kausality-io/kausality/blob/main/LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License"></a>
</p>

<p align="center">
  <a href="#overview">Overview</a> •
  <a href="#how-it-works">How It Works</a> •
  <a href="#installation">Installation</a> •
  <a href="#configuration">Configuration</a> •
  <a href="#development">Development</a>
</p>

---

## Overview

Kausality detects unexpected infrastructure changes (drift) in Kubernetes by monitoring when controllers mutate resources without an explicit spec change. It distinguishes between:

- **Expected changes**: Triggered by spec changes (`generation != observedGeneration`)
- **Unexpected changes**: Triggered by drift, external modifications, or software updates (`generation == observedGeneration`)

The system supports two modes:
- **Log mode** (default): Drift is detected and logged, warnings returned, but mutations are allowed
- **Enforce mode**: Drift without approval is denied

See [Configuration](#drift-detection-mode) for per-resource mode settings.

## How It Works

When a mutation is intercepted, Kausality:

1. **Identifies the actor** via user hash tracking — correlating users who update parent status with users who update child spec

2. **Checks the parent's state** (`generation` vs `observedGeneration`)

| Actor | Parent State | Result |
|-------|--------------|--------|
| Controller | gen != obsGen | **Expected** — controller is reconciling |
| Controller | gen == obsGen | **Drift** — controller changing without spec change |
| Different actor | any | **New origin** — not drift, just a different causal chain |

**Drift** specifically means: the controller is making changes when the parent spec hasn't changed. This could indicate:
- External modifications to cloud resources
- Updates to referenced resources (ClusterRelease, MachineClass)
- Controller behavior changes from software updates

**Different actors** (kubectl, HPA, GitOps tools) are not considered drift — they start new causal chains and are currently allowed. A planned ApprovalPolicy CRD will enable restricting certain actors.

### Lifecycle Phases

Kausality handles different lifecycle phases:

| Phase | Behavior |
|-------|----------|
| **Initializing** | Allow all changes (resource is being set up) |
| **Initialized** | Drift detection applies |
| **Deleting** | Allow all changes (cleanup phase) |

The `kausality.io/phase` annotation tracks the lifecycle phase:
- `initializing` — Resource not yet ready
- `initialized` — Resource reached steady state (persisted, never downgraded)
- `deleting` — Not stored; derived from `deletionTimestamp`

Initialization is detected by checking (in order):
1. `kausality.io/phase: "initialized"` annotation
2. `Initialized=True` condition
3. `Ready=True` condition
4. `status.observedGeneration` matches `generation` AND (`Synced=True` OR `Ready=True`)

### Trace Labels

Attach custom metadata to trace entries via `kausality.io/trace-*` annotations:

```yaml
metadata:
  annotations:
    kausality.io/trace-ticket: "JIRA-123"
    kausality.io/trace-pr: "567"
```

These become labels in the trace hop, allowing correlation with external systems:

```json
{"kind": "Deployment", "name": "prod", "labels": {"ticket": "JIRA-123", "pr": "567"}, ...}
```

## Installation

### Prerequisites

- Kubernetes 1.25+
- Helm 3.0+
- cert-manager (optional, for TLS certificate management)

### Using Helm

```bash
# Add the Helm repository (coming soon)
# helm repo add kausality https://kausality-io.github.io/kausality

# Install from source
helm install kausality ./charts/kausality \
  --namespace kausality-system \
  --create-namespace
```

## Configuration

Kausality uses Custom Resource Definitions (CRDs) to configure drift detection policies. This approach allows dynamic policy management without redeploying the webhook.

### Quick Start

After installing Kausality, create a policy to enable drift detection:

```yaml
apiVersion: kausality.io/v1alpha1
kind: Kausality
metadata:
  name: apps-policy
spec:
  # Which resources to track
  resources:
    - apiGroups: ["apps"]
      resources: ["deployments", "replicasets", "statefulsets"]
  # Drift detection mode: log or enforce
  mode: log
```

Apply the policy:

```bash
kubectl apply -f policy.yaml
```

### Kausality CRD

The `Kausality` CRD defines which resources to monitor and how to handle drift.

```yaml
apiVersion: kausality.io/v1alpha1
kind: Kausality
metadata:
  name: production-policy
spec:
  # Resources to track (required)
  resources:
    # Track all resources in apps group except DaemonSets
    - apiGroups: ["apps"]
      resources: ["*"]
      excluded: ["daemonsets"]
    # Track specific custom resources
    - apiGroups: ["example.com"]
      resources: ["ekscluster", "nodepools"]

  # Namespace filtering (optional)
  # If omitted, all namespaces are tracked (except system namespaces)
  namespaces:
    # Explicit list of namespaces
    names: ["production", "staging"]
    # Or use label selector (mutually exclusive with names)
    # selector:
    #   matchLabels:
    #     env: production
    # Always excluded, even if matching above
    excluded: ["kube-system"]

  # Object label selector (optional)
  # Only track objects matching these labels
  objectSelector:
    matchLabels:
      managed-by: kausality

  # Default mode: log or enforce
  mode: log

  # Per-resource/namespace mode overrides (optional)
  # First match wins
  overrides:
    - namespaces: ["production"]
      mode: enforce
    - apiGroups: ["apps"]
      resources: ["deployments"]
      namespaces: ["staging"]
      mode: enforce
```

### Policy Fields

| Field | Description |
|-------|-------------|
| `resources` | List of API groups and resources to track. Use `"*"` for all resources in a group. |
| `resources[].excluded` | Resources to exclude when using `"*"`. Only valid with wildcard. |
| `namespaces.names` | Explicit namespace list. Mutually exclusive with `selector`. |
| `namespaces.selector` | Label selector for namespaces. Mutually exclusive with `names`. |
| `namespaces.excluded` | Namespaces to always skip, even if matching above. |
| `objectSelector` | Only track objects matching these labels. |
| `mode` | Default mode: `log` (detect and warn) or `enforce` (block drift). |
| `overrides` | Fine-grained mode overrides by namespace/resource. First match wins. |

### Drift Detection Modes

| Mode | Behavior |
|------|----------|
| `log` | Drift is detected and logged, warnings added to response, but requests are allowed |
| `enforce` | Drift without approval is denied with an error message |

In `log` mode, drift warnings are returned via the admission response `warnings` field, which kubectl and other clients display to users.

### Multiple Policies

Multiple `Kausality` resources can coexist. When policies overlap, the most specific policy wins based on:
1. Explicit namespace names over selectors
2. Specific resource lists over wildcards
3. Object selectors over no selectors

### Helm Values

See [charts/kausality/values.yaml](charts/kausality/values.yaml) for Helm configuration options (replicas, resources, certificates, etc.). Policy configuration is done via CRDs, not Helm values.

### Controller and Security

The Kausality controller automatically manages webhook configuration and RBAC based on your policies. It requires broad permissions (`*/*`) to create per-policy ClusterRoles.

This is not an additional security risk: the controller already manages `MutatingWebhookConfiguration`, which can intercept any API request — effectively privileged access.

**Disabling the controller:** If your security posture requires it, disable the controller and manage webhook rules and RBAC manually:

```yaml
# values.yaml
controller:
  enabled: false
```

See [Kausality CRD Design](doc/design/KAUSALITY_CRD.md#disabling-the-controller) for manual configuration examples.

## Development

### Prerequisites

- Go 1.25+
- Docker (for building images)
- kubectl configured for a test cluster
- [kind](https://kind.sigs.k8s.io/) (optional, for local testing)

### Building

```bash
# Build the webhook binary
make build

# Run tests
make test

# Run linter
make lint

# Build Docker image
make docker-build
```

### Running Locally

```bash
# Run the webhook locally (requires kubeconfig)
make run
```

### Testing

```bash
# Run unit tests
make test

# Run envtest integration tests (real API server)
make envtest

# Run tests with verbose output
make test-verbose
```

### Project Structure

```
kausality/
├── api/
│   └── v1alpha1/                # CRD types (Kausality)
├── cmd/
│   ├── kausality-webhook/       # Admission webhook server
│   ├── kausality-controller/    # Policy controller (reconciles CRDs, manages webhook config)
│   ├── kausality-backend-log/   # Backend that logs DriftReports as YAML
│   └── kausality-backend-tui/   # Backend with interactive TUI
├── pkg/
│   ├── admission/               # Admission webhook handler
│   ├── approval/                # Approval/rejection annotation handling
│   ├── backend/                 # Backend server and TUI components
│   ├── callback/                # Drift notification webhook callbacks
│   │   └── v1alpha1/            # DriftReport API types
│   ├── config/                  # Configuration types and loading
│   ├── controller/              # Controller identification via user hash tracking
│   ├── drift/                   # Core drift detection logic
│   ├── policy/                  # Policy controller and store
│   ├── trace/                   # Request trace propagation
│   └── webhook/                 # Webhook server
├── charts/
│   └── kausality/               # Helm chart
│       └── crds/                # CRD manifests
└── doc/
    └── design/                  # Design specification
```

### Architecture

Kausality consists of two main components:

| Component | Binary | Purpose |
|-----------|--------|---------|
| **Webhook** | `kausality-webhook` | Admission webhook that intercepts mutations and detects drift |
| **Controller** | `kausality-controller` | Watches Kausality CRDs and reconciles webhook configuration |

**RBAC:**

| ClusterRole | Purpose |
|-------------|---------|
| `kausality-webhook` | Read Kausality policies and namespaces |
| `kausality-webhook-resources` | Aggregated access to tracked resources (populated by controller) |
| `kausality-controller` | Manage CRDs, webhook config, and per-policy ClusterRoles |

## Documentation

**Design:**
- [Design Overview](doc/design/INDEX.md) — Core concepts and quick reference
- [Kausality CRD](doc/design/KAUSALITY_CRD.md) — Policy configuration, resource selection, precedence rules
- [Drift Detection](doc/design/DRIFT_DETECTION.md) — Controller identification, annotation protection, lifecycle phases
- [Approvals](doc/design/APPROVALS.md) — Approval/rejection annotations, modes, freeze/snooze
- [Tracing](doc/design/TRACING.md) — Request trace propagation through controller hierarchy
- [Callbacks](doc/design/CALLBACKS.md) — Drift notification webhooks, DriftReport API
- [Deployment](doc/design/DEPLOYMENT.md) — Webhook deployment, Helm configuration

**Reference:**
- [Architecture Decisions](doc/ADR.md) — Rationale, trade-offs, alternatives
- [Roadmap](doc/ROADMAP.md) — Implementation phases and status

## Roadmap

- [x] **Phase 1**: Logging only
  - Detect drift via generation/observedGeneration comparison
  - Controller identification via user hash tracking (correlates status updaters with spec updaters)
  - Log when drift would be blocked
  - Support both library mode and webhook mode

- [x] **Phase 2**: Request-trace annotation propagation
  - Trace causal chain through controller hierarchy
  - New origins for different actors, extend for controller hops

- [x] **Phase 3**: Per-object approval annotations
  - [x] Approval types (once, generation, always)
  - [x] Rejection support
  - [x] Approval checking in handler
  - [x] Enforce mode (per-G/GR configuration with label selectors)
  - [x] Approval pruning (mode=once consumed after use)

- [x] **Phase 4**: Drift notification webhook callbacks
  - [x] DriftReport API (kausality.io/v1alpha1)
  - [x] Content-based deduplication
  - [x] Backend implementations (log, TUI)
  - [x] Helm chart integration

- [ ] **Phase 5**: Kausality CRD (in progress)
  - [x] Kausality CRD for policy configuration
  - [x] CEL validation rules
  - [x] Policy controller for webhook configuration
  - [x] Automatic RBAC generation (per-policy ClusterRoles with aggregation)
  - [ ] E2E tests with CRD-based policies

- [ ] **Phase 6**: Slack integration

## License

Apache 2.0 — see [LICENSE](LICENSE) for details.
