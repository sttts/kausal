<p align="center">
  <img src="logo.png" alt="Kausality Logo" width="200" />
</p>

<h1 align="center">Kausality</h1>

<p align="center">
  <em>"Every mutation needs a cause."</em>
</p>

<p align="center">
  <strong>Causal traceability for Kubernetes resource mutations</strong>
</p>

<p align="center">
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

**Phase 1 (current)**: Logging only — drift is detected and logged, but mutations are not blocked.

**Future phases** will add approval workflows, Slack integration, and policy-based exceptions.

## How It Works

When a controller mutates a child object, Kausality checks the **parent's** state:

```
parent := resolve via controller ownerReference (controller: true)

if parent.generation != parent.status.observedGeneration:
    # Parent spec changed → expected change → ALLOW
else:
    # Parent spec unchanged → drift detected → LOG
```

**Why this works**: Controllers reconcile children based on their own spec. If the parent's spec hasn't changed (generation == observedGeneration), any child mutation indicates drift.

### Lifecycle Phases

Kausality handles different lifecycle phases:

| Phase | Behavior |
|-------|----------|
| **Initializing** | Allow all changes (resource is being set up) |
| **Ready** | Drift detection applies |
| **Deleting** | Allow all changes (cleanup phase) |

Initialization is detected by checking (in order):
1. `Initialized=True` condition
2. `Ready=True` condition
3. `status.observedGeneration` exists

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
# Run all tests
make test

# Run tests with verbose output
make test-verbose
```

### Project Structure

```
kausality/
├── cmd/
│   └── kausality-webhook/    # Webhook server binary
├── pkg/
│   ├── drift/                # Core drift detection logic
│   │   ├── types.go          # DriftResult, ParentState types
│   │   ├── detector.go       # Main drift detection
│   │   ├── resolver.go       # Parent object resolution
│   │   └── lifecycle.go      # Lifecycle phase detection
│   ├── admission/            # Admission webhook handler
│   └── webhook/              # Webhook server
├── charts/
│   └── kausality/            # Helm chart
└── doc/
    └── DESIGN.md             # Design specification
```

## Documentation

- [Design Document](doc/DESIGN.md) — Full design specification for admission-only drift detection

## Roadmap

- [x] **Phase 1**: Logging only (current)
  - Detect drift via generation/observedGeneration comparison
  - Log when drift would be blocked
  - Support both library mode and webhook mode

- [ ] **Phase 2**: Request-trace annotation propagation
- [ ] **Phase 3**: Per-object approval annotations
- [ ] **Phase 4**: ApprovalPolicy CRD and Slack integration
- [ ] **Phase 5**: TerraformApprovalPolicy for L0 controllers

## License

Apache 2.0 — see [LICENSE](LICENSE) for details.
