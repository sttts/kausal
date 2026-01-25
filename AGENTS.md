# Kausality Development Guidelines

This document provides guidance for working with code in this repository.

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

- **`pkg/approval/`** - Approval/rejection annotation handling
  - `types.go` - `Approval`, `Rejection`, `ChildRef` types
  - `checker.go` - Checks approvals against child references
  - `pruner.go` - Prunes consumed and stale approvals

- **`pkg/config/`** - Configuration handling
  - `config.go` - Per-resource enforce mode configuration

- **`pkg/callback/`** - Drift notification webhook callbacks
  - `v1alpha1/types.go` - `DriftReport`, `DriftReportResponse`, `ObjectReference`, `RequestContext`
  - `sender.go` - HTTP client for sending DriftReports to webhook endpoints
  - `tracker.go` - ID tracking for deduplication

- **`pkg/backend/`** - Backend server implementations
  - `server.go` - HTTP server with in-memory drift store
  - `store.go` - Thread-safe drift report storage

- **`pkg/testing/`** - Test helpers
  - `eventually.go` - Eventually helpers with verbose YAML logging

- **`pkg/webhook/`** - HTTP server for ValidatingAdmissionWebhook

### Key Design Decisions

1. **Controller identification via managedFields**: The manager who owns `f:status.f:observedGeneration` is the controller. Compare `request.options.fieldManager` with this.

2. **Spec changes only**: Only intercepts mutations to `spec`. Status and metadata changes are ignored.

3. **Lifecycle phases**: Initializing and Deleting phases allow all changes. Detection uses `observedGeneration` existence, `Initialized`/`Ready` conditions.

4. **Phase 1 = logging only**: Currently detects and logs drift but doesn't block. `Allowed` is always `true`.

## Test Conventions

### Libraries

Use the following testing libraries consistently across all tests:

- **github.com/stretchr/testify** - For assertions (`assert`, `require`)
- **github.com/google/go-cmp** - For comparing complex objects with readable diffs

### Assertions

Use `assert` for non-fatal assertions and `require` for fatal assertions:

```go
import (
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestExample(t *testing.T) {
    // Use require when the test cannot continue if the assertion fails
    result, err := doSomething()
    require.NoError(t, err)
    require.NotNil(t, result)

    // Use assert for non-fatal checks
    assert.Equal(t, expected, result.Value)
    assert.Len(t, result.Items, 3)
}
```

### Object Comparison

Use `cmp.Diff` from go-cmp for comparing complex objects:

```go
import "github.com/google/go-cmp/cmp"

func TestObjectComparison(t *testing.T) {
    want := SomeStruct{...}
    got := computeResult()

    if diff := cmp.Diff(want, got); diff != "" {
        t.Errorf("Result mismatch (-want +got):\n%s", diff)
    }
}
```

### Eventually Helpers

For tests that need to wait for conditions, use the helpers in `pkg/testing`:

```go
import ktesting "github.com/kausality-io/kausality/pkg/testing"

func TestEventualCondition(t *testing.T) {
    // Wait for an unstructured object to meet a condition
    ktesting.EventuallyUnstructured(t,
        func() (*unstructured.Unstructured, error) {
            return client.Get(ctx, name, namespace)
        },
        ktesting.HasCondition("Ready", metav1.ConditionTrue),
        30*time.Second,
        100*time.Millisecond,
        "waiting for object to become ready",
    )
}
```

The `Eventually` helpers log verbose context (including YAML representation of objects) when conditions are not met, making test failures easier to debug.

Available check functions:
- `HasCondition(type, status)` - Check status conditions
- `HasGeneration(gen)` - Check object generation
- `HasObservedGeneration()` - Check observedGeneration equals generation
- `HasAnnotation(key, value)` - Check annotation presence and value
- `Not(check)` - Negate any check function
- `ToYAML(obj)` - Convert object to YAML for logging

### Verbose Logging

When assertions fail in eventually loops, provide helpful context. The `pkg/testing` helpers automatically include YAML representation of objects:

```go
ktesting.EventuallyUnstructured(t, getter,
    func(obj *unstructured.Unstructured) (bool, string) {
        // Return a descriptive reason string
        if obj.GetGeneration() != expected {
            return false, fmt.Sprintf(
                "generation is %d, waiting for %d",
                obj.GetGeneration(), expected,
            )
        }
        return true, "generation matches"
    },
    timeout, tick,
)
```

### Table-Driven Tests

Use table-driven tests with descriptive test names:

```go
func TestFeature(t *testing.T) {
    tests := []struct {
        name    string
        input   Input
        want    Output
        wantErr bool
    }{
        {
            name:  "valid input produces expected output",
            input: Input{...},
            want:  Output{...},
        },
        {
            name:    "invalid input returns error",
            input:   Input{...},
            wantErr: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := Process(tt.input)
            if tt.wantErr {
                assert.Error(t, err)
                return
            }
            require.NoError(t, err)
            assert.Equal(t, tt.want, got)
        })
    }
}
```

### Envtest Notes

Envtests use a shared environment in `TestMain` for speed. When writing envtests:
- Use FQDN finalizers (e.g., `test.kausality.io/finalizer`)
- Set `APIVersion` and `Kind` on objects before JSON marshaling (TypeMeta isn't populated by `client.Get`)

### E2E Tests

E2E tests run against a real Kubernetes cluster (kind). They are located in `test/e2e/` and use the build tag `//go:build e2e`.

**Running E2E tests:**

```bash
# Run the full e2e test suite (creates kind cluster, deploys kausality, runs tests)
./test/e2e/run.sh

# Run Go e2e tests directly (requires cluster with kausality already deployed)
go test ./test/e2e/kubernetes -tags=e2e -v
```

**E2E test conventions:**

1. **Use `ktesting.Eventually`** - Never use `require.Eventually`. The `ktesting.Eventually` helper provides better debugging output with descriptive reason strings:

```go
ktesting.Eventually(t, func() (bool, string) {
    pod, err := clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
    if err != nil {
        return false, fmt.Sprintf("error getting pod: %v", err)
    }
    if pod.Status.Phase != corev1.PodRunning {
        return false, fmt.Sprintf("pod phase=%s, waiting for Running", pod.Status.Phase)
    }
    return true, "pod is running"
}, timeout, interval, "pod should be running")
```

2. **Reentrant tests** - Tests must be reentrant (can run multiple times). Use random namespace names:

```go
func TestMain(m *testing.M) {
    testNamespace = fmt.Sprintf("e2e-test-%s", rand.String(6))
    // Create namespace, run tests, cleanup
}
```

3. **Story-telling with t.Log** - Tests should tell a story with descriptive log statements:

```go
func TestFeature(t *testing.T) {
    t.Log("=== Testing Feature X ===")
    t.Log("When condition Y, the system should do Z")

    t.Log("")
    t.Log("Step 1: Creating the resource...")
    // ... create resource

    t.Log("")
    t.Log("Step 2: Waiting for controller to reconcile...")
    // ... wait for condition

    t.Log("")
    t.Log("SUCCESS: Feature X works correctly")
}
```

4. **Separate TestMain** - Put `TestMain` in a separate `test_main.go` file:

```
test/e2e/kubernetes/
├── test_main.go    # TestMain, shared variables (clientset, testNamespace)
└── e2e_test.go     # Test functions
```

5. **Use testify assertions** - Use `require` for fatal assertions, `assert` for non-fatal:

```go
_, err := clientset.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
require.NoError(t, err)

assert.Equal(t, "expected", actual)
```

6. **How to cause drift for testing** - Drift is when a controller updates a child while the parent is stable. To trigger drift in tests:
   - Change the **spec** of a child resource (not annotations/labels)
   - Or delete a child resource
   - This causes the parent controller to reconcile and try to correct the drift
   - The controller's correction attempt is what kausality detects as drift
   - Important: User modifications are NOT drift - they're a new causal origin. Only the controller's subsequent correction is drift.

## Commit Conventions

Follow the commit message format:

```
area/subarea: short description

Longer description if needed.

Co-Authored-By: Claude Opus 4.5 <noreply@anthropic.com>
```

Example areas:
- `admission`: Admission webhook handler
- `approval`: Approval/rejection system
- `config`: Configuration handling
- `drift`: Drift detection
- `trace`: Request trace propagation
- `webhook`: Webhook server
- `doc`: Documentation
- `test`: Test improvements

## Testing Requirements

**CRITICAL: Tests MUST be green before marking any task complete. NO EXCEPTIONS.**

A task involving code or test changes is NOT done until:
1. All relevant tests pass (unit, envtest, or e2e as appropriate)
2. You have actually run the tests and seen them pass
3. Never mark a task complete based on "should work" - verify it works

**You MUST run tests before committing any code changes.** There is no value in adding or changing tests without verifying they pass.

- **Unit tests**: Run `make test` for any code changes
- **Envtests**: Run `make envtest` when changing admission/drift logic
- **E2E tests**: Run against a local kind cluster during development:
  ```bash
  # Development: run tests against existing local kind cluster
  go test ./test/e2e/crossplane -tags=e2e -v

  # CI only: full setup with new cluster
  ./test/e2e/crossplane/run.sh
  ```

Never commit test changes without running them first.

## Code Style

- Keep functions focused and small
- Prefer explicit over implicit
- Avoid over-engineering - implement what's needed now
- Add comments only where the logic isn't self-evident
- Preserve existing formatting unless changing semantics
