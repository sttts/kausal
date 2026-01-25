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

Initialization is detected by checking (in order):
1. `Initialized=True` condition
2. `Ready=True` condition
3. `status.observedGeneration` exists

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

### Configuration

See [charts/kausality/values.yaml](charts/kausality/values.yaml) for all available options.

Key configuration options:

```yaml
# Which resources to intercept
resourceRules:
  include:
    - apiGroups: ["apps"]
      resources: ["deployments", "replicasets"]
    - apiGroups: ["example.com"]
      resources: ["ekscluster", "nodepools"]

# Namespaces to exclude
excludeNamespaces:
  - kube-system
  - kube-public

# Certificate management
certificates:
  certManager:
    enabled: true
    issuerRef:
      name: letsencrypt-prod
      kind: ClusterIssuer
```

## Configuration

### Resource Targeting

Configure which resources are subject to drift detection via `resourceRules`:

```yaml
resourceRules:
  include:
    # All resources in apps group
    - apiGroups: ["apps"]
      resources: ["*"]
    # Specific custom resources
    - apiGroups: ["example.com"]
      resources: ["ekscluster", "nodepools"]
  exclude:
    # Skip ConfigMaps and Secrets
    - apiGroups: [""]
      resources: ["configmaps", "secrets"]
```

### Namespace Exclusions

Exclude entire namespaces from drift detection:

```yaml
excludeNamespaces:
  - kube-system
  - kube-public
  - kube-node-lease
```

### Drift Detection Mode

Configure whether drift is logged only or enforced (requests blocked).

#### Global Default (Helm)

Set the global default mode in values.yaml:

```yaml
driftDetection:
  # Default mode for all resources: "log" or "enforce"
  defaultMode: log
```

#### Runtime Configuration (Annotations)

Override the mode at runtime using the `kausality.io/mode` annotation on namespaces or objects:

```yaml
# Enforce mode for an entire namespace
apiVersion: v1
kind: Namespace
metadata:
  name: production
  annotations:
    kausality.io/mode: "enforce"
---
# Log mode override for a specific object
apiVersion: apps/v1
kind: Deployment
metadata:
  name: experimental
  namespace: production
  annotations:
    kausality.io/mode: "log"  # Override namespace's enforce mode
```

**Precedence (most specific wins):**
1. Object annotation `kausality.io/mode`
2. Namespace annotation `kausality.io/mode`
3. Global default from Helm values (`defaultMode`)

| Mode | Behavior |
|------|----------|
| `log` | Drift is detected and logged, warnings added to response, but requests are allowed |
| `enforce` | Drift without approval is denied with an error message |

In `log` mode, drift warnings are returned via the admission response `warnings` field, which kubectl and other clients display to users.

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
├── cmd/
│   ├── kausality-webhook/       # Webhook server binary
│   ├── kausality-backend-log/   # Backend that logs DriftReports as YAML
│   └── kausality-backend-tui/   # Backend with interactive TUI
├── pkg/
│   ├── admission/               # Admission webhook handler
│   ├── approval/                # Approval/rejection annotation handling
│   ├── backend/                 # Backend server and TUI components
│   ├── callback/                # Drift notification webhook callbacks
│   │   ├── v1alpha1/            # DriftReport API types
│   │   ├── sender.go            # HTTP client for webhook calls
│   │   └── tracker.go           # ID tracking for deduplication
│   ├── config/                  # Configuration types and loading
│   ├── controller/              # Controller identification via user hash tracking
│   ├── drift/                   # Core drift detection logic
│   │   ├── types.go             # DriftResult, ParentState types
│   │   ├── detector.go          # Main drift detection
│   │   ├── resolver.go          # Parent object resolution
│   │   └── lifecycle.go         # Lifecycle phase detection
│   ├── trace/                   # Request trace propagation
│   └── webhook/                 # Webhook server
├── charts/
│   └── kausality/               # Helm chart
└── doc/
    └── design/                  # Design specification
```

## Documentation

**Design:**
- [Design Overview](doc/design/INDEX.md) — Core concepts and quick reference
- [Drift Detection](doc/design/DRIFT_DETECTION.md) — Controller identification, annotation protection, lifecycle phases
- [Approvals](doc/design/APPROVALS.md) — Approval/rejection annotations, modes, freeze/snooze
- [Tracing](doc/design/TRACING.md) — Request trace propagation through controller hierarchy
- [Callbacks](doc/design/CALLBACKS.md) — Drift notification webhooks, DriftReport API
- [Deployment](doc/design/DEPLOYMENT.md) — Library vs webhook mode, Helm configuration

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

- [ ] **Phase 5**: ApprovalPolicy CRD
- [ ] **Phase 6**: Slack integration

## License

Apache 2.0 — see [LICENSE](LICENSE) for details.
