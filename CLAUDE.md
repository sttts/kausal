# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and Test Commands

```bash
make build          # Build webhook binary to bin/kausality-webhook
make test           # Run unit tests with coverage
make envtest        # Run envtest integration tests (real API server)
make lint           # Run golangci-lint
make lint-fix       # Run golangci-lint with auto-fix

# Run a single test
go test ./pkg/drift -run TestDetectFromState -v

# Run envtests only
go test ./pkg/admission -tags=envtest -run TestDriftDetection -v
```

## Architecture

Kausality is an admission-based drift detection system for Kubernetes. It detects when controllers make unexpected changes to child resources.

### Core Concept: Drift Detection

**Drift** = controller making changes when parent hasn't changed (`generation == observedGeneration`)

The system identifies the controller by checking who owns `status.observedGeneration` in the parent's `managedFields`, then compares with the request's `fieldManager`.

| Actor | Parent State | Result |
|-------|--------------|--------|
| Controller | gen != obsGen | Expected (reconciling) |
| Controller | gen == obsGen | **Drift** |
| Different actor | any | New causal origin (not drift) |

### Package Structure

- **`pkg/drift/`** - Core drift detection logic
  - `detector.go` - Main `Detector` with `DetectWithFieldManager()`
  - `resolver.go` - Resolves parent via controller ownerRef, extracts `ParentState` including `ControllerManager` from managedFields
  - `lifecycle.go` - Detects phases: Initializing, Ready, Deleting
  - `types.go` - `DriftResult`, `ParentState`, `ParentRef`

- **`pkg/trace/`** - Causal trace propagation
  - `propagator.go` - `PropagateWithFieldManager()` decides origin vs extend
  - `types.go` - `Trace`, `Hop` types with JSON serialization

- **`pkg/admission/`** - Admission webhook handler
  - `handler.go` - Wraps drift detector + trace propagator for admission requests
  - `handler_envtest_test.go` - Comprehensive envtests against real API server

- **`pkg/webhook/`** - HTTP server for ValidatingAdmissionWebhook

### Key Design Decisions

1. **Controller identification via managedFields**: The manager who owns `f:status.f:observedGeneration` is the controller. Compare `request.options.fieldManager` with this.

2. **Spec changes only**: Only intercepts mutations to `spec`. Status and metadata changes are ignored.

3. **Lifecycle phases**: Initializing and Deleting phases allow all changes. Detection uses `observedGeneration` existence, `Initialized`/`Ready` conditions.

4. **Phase 1 = logging only**: Currently detects and logs drift but doesn't block. `Allowed` is always `true`.

## Envtest Notes

Envtests use a shared environment in `TestMain` for speed. When writing envtests:
- Use FQDN finalizers (e.g., `test.kausality.io/finalizer`)
- Set `APIVersion` and `Kind` on objects before JSON marshaling (TypeMeta isn't populated by `client.Get`)
