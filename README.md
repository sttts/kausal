<p align="center">
  <img src="logo.png" alt="Kausality Logo" width="200" />
</p>

<h1 align="center">Kausality</h1>

<p align="center">
  <em>"Every mutation needs a cause."</em>
</p>

<p align="center">
  <strong>Drift detection for Kubernetes — know when your infrastructure changes unexpectedly</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Status-Experimental-orange.svg" alt="Experimental">
  <a href="https://github.com/kausality-io/kausality/actions/workflows/ci.yaml"><img src="https://github.com/kausality-io/kausality/actions/workflows/ci.yaml/badge.svg" alt="CI"></a>
  <a href="https://goreportcard.com/report/github.com/kausality-io/kausality"><img src="https://goreportcard.com/badge/github.com/kausality-io/kausality" alt="Go Report Card"></a>
  <a href="https://github.com/kausality-io/kausality/blob/main/LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick Start</a> •
  <a href="#what-is-drift">What is Drift?</a> •
  <a href="#configuration">Configuration</a> •
  <a href="#documentation">Documentation</a>
</p>

---

> **⚠️ Experimental**: This project is under active development. APIs may change without notice. Not recommended for production use yet.

## Why Kausality?

Ever had your infrastructure change without anyone touching it? Controllers reconciling, external resources drifting, software updates causing unexpected mutations — Kausality catches these.

**The problem**: Kubernetes controllers constantly reconcile resources. When something external changes (cloud state, referenced configs, controller behavior), your resources change too — silently, without audit trail.

**The solution**: Kausality intercepts mutations and asks: "Did someone actually request this change, or is this drift?" If it's drift, you get notified (or can block it).

## Quick Start

### 1. Install Kausality

```bash
# Clone the repository
git clone https://github.com/kausality-io/kausality.git
cd kausality

# Install with Helm
helm install kausality ./charts/kausality \
  --namespace kausality-system \
  --create-namespace
```

### 2. Create a Policy

Tell Kausality what to watch. Save this as `policy.yaml`:

```yaml
apiVersion: kausality.io/v1alpha1
kind: Kausality
metadata:
  name: watch-apps
spec:
  resources:
    - apiGroups: ["apps"]
      resources: ["deployments", "replicasets"]
  mode: log  # Detect and warn, don't block
```

```bash
kubectl apply -f policy.yaml
```

### 3. See It In Action

Create a Deployment and wait for it to stabilize:

```bash
kubectl create deployment nginx --image=nginx:1.24
kubectl rollout status deployment/nginx
```

Now cause drift by directly modifying the ReplicaSet:

```bash
# Get the ReplicaSet name
RS=$(kubectl get rs -l app=nginx -o jsonpath='{.items[0].metadata.name}')

# Directly modify the ReplicaSet (this is drift!)
kubectl patch rs $RS --type=merge -p '{"spec":{"replicas":3}}'
```

You'll see a warning:

```
Warning: [kausality] drift detected: controller updating ReplicaSet while parent Deployment is stable
```

The controller will try to "fix" this back to the original replica count — and Kausality detects that as drift too, because the Deployment spec hasn't changed.

### 4. Enable Drift Logging (Optional)

To see detailed DriftReports, enable a backend:

```bash
# Log backend - outputs DriftReports as YAML to stdout
helm upgrade kausality ./charts/kausality \
  --namespace kausality-system \
  --set backend.enabled=true

# View the drift logs
kubectl logs -n kausality-system -l app.kubernetes.io/component=backend-log -f
```

**TUI backend** - interactive terminal UI for real-time drift monitoring:

```bash
helm upgrade kausality ./charts/kausality \
  --namespace kausality-system \
  --set backendTui.enabled=true

# Attach to the TUI
kubectl attach -n kausality-system deploy/kausality-backend-tui -it
```

---

## What is Drift?

**Drift** = a controller making changes when nothing requested that change.

Kausality distinguishes between:

| Scenario | What Happens | Is it Drift? |
|----------|--------------|--------------|
| You update a Deployment | Controller creates new ReplicaSet | **No** — expected reconciliation |
| Cloud resource changes externally | Controller updates child resources | **Yes** — no spec change triggered this |
| You directly edit a ReplicaSet | Controller reverts your change | **Yes** — controller acting without spec change |
| Software update changes controller behavior | Controller re-reconciles differently | **Yes** — silent change |

### How Detection Works

Kausality tracks who updates what:

1. **Parent status updates** → records the controller's identity
2. **Child spec updates** → checks if this is the same controller
3. **Stable parent** (`generation == observedGeneration`) + **controller update** = **drift**

Different actors (you running `kubectl`, HPA, GitOps tools) are *not* drift — they start new causal chains.

---

## Configuration

### Kausality CRD

The `Kausality` CRD defines what to monitor and how:

```yaml
apiVersion: kausality.io/v1alpha1
kind: Kausality
metadata:
  name: production-policy
spec:
  # What resources to watch
  resources:
    - apiGroups: ["apps"]
      resources: ["deployments", "replicasets", "statefulsets"]
    - apiGroups: ["example.com"]
      resources: ["*"]
      excluded: ["unwanted-resource"]

  # Which namespaces (optional — defaults to all)
  namespaces:
    names: ["production", "staging"]
    # Or use selectors:
    # selector:
    #   matchLabels:
    #     env: production
    excluded: ["kube-system"]

  # Filter by object labels (optional)
  objectSelector:
    matchLabels:
      managed-by: kausality

  # Mode: log or enforce
  mode: log

  # Override mode for specific cases
  overrides:
    - namespaces: ["production"]
      mode: enforce
```

### Modes

| Mode | Behavior |
|------|----------|
| `log` | Detect drift, return warnings, allow the mutation |
| `enforce` | Detect drift, **block** mutations without approval |

### Approvals (Enforce Mode)

In `enforce` mode, you can approve specific drift:

```yaml
# On the parent resource
metadata:
  annotations:
    kausality.io/approvals: |
      [{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"nginx-abc123","mode":"always"}]
```

Approval modes:
- `always` — permanently approved
- `once` — consumed after first use
- `generation` — valid until parent generation changes

---

## How It Works

### Architecture

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│   API Server    │────▶│  Kausality       │────▶│  Backend        │
│                 │     │  Webhook         │     │  (optional)     │
└─────────────────┘     └──────────────────┘     └─────────────────┘
                               │
                               ▼
                        ┌──────────────────┐
                        │  Kausality       │
                        │  Controller      │
                        └──────────────────┘
                               │
                               ▼
                        ┌──────────────────┐
                        │  Kausality CRDs  │
                        │  (policies)      │
                        └──────────────────┘
```

| Component | Purpose |
|-----------|---------|
| **Webhook** | Intercepts mutations, detects drift, adds warnings/blocks |
| **Controller** | Watches Kausality CRDs, configures webhook rules and RBAC |
| **Backend** | Receives DriftReports for logging/alerting (optional) |

### Lifecycle Phases

Kausality handles resource lifecycle:

| Phase | Behavior |
|-------|----------|
| **Initializing** | Allow all changes (resource setting up) |
| **Initialized** | Drift detection active |
| **Deleting** | Allow all changes (cleanup) |

Initialization is detected via:
- `kausality.io/phase: initialized` annotation
- `Initialized=True` or `Ready=True` conditions
- `observedGeneration` matching `generation`

### Trace Labels

Attach metadata to trace entries for correlation:

```yaml
metadata:
  annotations:
    kausality.io/trace-ticket: "JIRA-123"
    kausality.io/trace-pr: "567"
```

---

## Installation Options

### Helm (Recommended)

```bash
helm install kausality ./charts/kausality \
  --namespace kausality-system \
  --create-namespace \
  --set backend.enabled=true  # Enable drift logging
```

### With cert-manager

```bash
helm install kausality ./charts/kausality \
  --namespace kausality-system \
  --create-namespace \
  --set certificates.selfSigned.enabled=false \
  --set certificates.certManager.enabled=true \
  --set certificates.certManager.issuerRef.name=my-issuer \
  --set certificates.certManager.issuerRef.kind=ClusterIssuer
```

### Key Helm Values

| Value | Default | Description |
|-------|---------|-------------|
| `backend.enabled` | `false` | Deploy the log backend (YAML to stdout) |
| `backendTui.enabled` | `false` | Deploy the TUI backend (interactive terminal) |
| `controller.enabled` | `true` | Auto-manage webhook config from CRDs |
| `certificates.selfSigned.enabled` | `true` | Use Helm-generated certs |
| `logging.level` | `info` | Log level (debug, info, warn, error) |

See [values.yaml](charts/kausality/values.yaml) for all options.

---

## Documentation

**Concepts:**
- [Design Overview](doc/design/INDEX.md) — Core concepts and architecture
- [Drift Detection](doc/design/DRIFT_DETECTION.md) — How controller identification works
- [Approvals](doc/design/APPROVALS.md) — Approval modes, rejections, freeze/snooze

**Reference:**
- [Kausality CRD](doc/design/KAUSALITY_CRD.md) — Full CRD specification
- [Callbacks](doc/design/CALLBACKS.md) — DriftReport API for integrations
- [Architecture Decisions](doc/ADR.md) — Design rationale and trade-offs

---

## Development

```bash
# Build
make build

# Run tests
make test

# Run envtest integration tests
make envtest

# Run E2E tests (requires cluster)
make e2e

# Lint
make lint
```

### Local Development with Tilt

```bash
# Start local development (auto-rebuilds on changes)
tilt up
```

### Project Structure

```
kausality/
├── api/v1alpha1/           # CRD types
├── cmd/
│   ├── kausality-webhook/      # Admission webhook
│   ├── kausality-controller/   # Policy controller
│   └── kausality-backend-*/    # Backend implementations
├── pkg/
│   ├── admission/          # Webhook handler
│   ├── drift/              # Core drift detection
│   ├── policy/             # Policy store and controller
│   └── ...
└── charts/kausality/       # Helm chart
```

---

## Roadmap

- [x] Drift detection via generation comparison
- [x] Controller identification via user hash tracking
- [x] Request trace propagation
- [x] Per-object approvals and rejections
- [x] Enforce mode with per-resource configuration
- [x] DriftReport callbacks and backends
- [x] Kausality CRD for policy configuration
- [ ] Slack integration
- [ ] UI dashboard

---

## License

Apache 2.0 — see [LICENSE](LICENSE).
