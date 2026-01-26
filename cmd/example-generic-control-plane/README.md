# Example Generic Control Plane

This example demonstrates how to embed kausality admission into a generic Kubernetes-style API server using `k8s.io/apiserver` and `kcp-dev/embeddedetcd`.

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                 Generic API Server                   │
├─────────────────────────────────────────────────────┤
│  Admission Chain                                     │
│  ┌─────────────────────────────────────────────────┐│
│  │              Kausality Admission                 ││
│  │  - Drift detection                              ││
│  │  - Trace propagation                            ││
│  │  - User hash tracking                           ││
│  └─────────────────────────────────────────────────┘│
├─────────────────────────────────────────────────────┤
│  API Groups                                          │
│  - example.kausality.io/v1alpha1 (Widget, WidgetSet)│
├─────────────────────────────────────────────────────┤
│  Storage: Embedded etcd                              │
└─────────────────────────────────────────────────────┘
```

## API Resources

### Widget

A simple namespaced resource:

```yaml
apiVersion: example.kausality.io/v1alpha1
kind: Widget
metadata:
  name: my-widget
  namespace: default
spec:
  color: blue
```

### WidgetSet

A parent resource that manages Widgets:

```yaml
apiVersion: example.kausality.io/v1alpha1
kind: WidgetSet
metadata:
  name: my-widgetset
  namespace: default
spec:
  replicas: 3
  template:
    color: blue
status:
  observedGeneration: 1
  readyWidgets: 3
```

## Key Concepts

### Static Policy Resolver

Instead of using the Kausality CRD for policy configuration, this example uses a `StaticResolver` that returns a fixed mode for all resources:

```go
policyResolver := policy.NewStaticResolver(kausalityv1alpha1.ModeEnforce)
```

### Kausality Admission Plugin

The kausality admission plugin wraps the standard kausality handler and adapts it to k8s.io/apiserver's admission interface:

```go
type KausalityAdmission struct {
    handler *kausalityAdmission.Handler
    scheme  *runtime.Scheme
    log     logr.Logger
}

func (k *KausalityAdmission) Handles(operation admission.Operation) bool {
    // Handle CREATE, UPDATE, DELETE
}

func (k *KausalityAdmission) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
    // Convert attributes to admission.Request and call handler
}
```

### Embedded etcd

The example uses `kcp-dev/embeddedetcd` to run etcd in-process, eliminating the need for a separate etcd cluster.

## Building

```bash
cd cmd/example-generic-control-plane
go build .
```

## Running

```bash
./example-generic-control-plane --data-dir=/tmp/example-cp
```

Options:
- `--data-dir` - Directory for etcd data and server state (default: `/tmp/example-control-plane`)
- `--bind-address` - Address to bind the API server (default: `127.0.0.1`)
- `--bind-port` - Port to bind the API server (default: `8443`)

## Testing

Run the smoke tests:

```bash
go test -v ./...
```

The tests verify:
- Widget CREATE gets kausality trace annotations
- Widget UPDATE without spec changes is allowed

## Using the API

Once running, you can interact with the API using kubectl:

```bash
# Create a kubeconfig (the server uses self-signed certs)
kubectl --kubeconfig=/dev/null \
  --server=https://127.0.0.1:8443 \
  --insecure-skip-tls-verify \
  create -f - <<EOF
apiVersion: example.kausality.io/v1alpha1
kind: Widget
metadata:
  name: test-widget
  namespace: default
spec:
  color: blue
EOF

# Get the widget to see kausality annotations
kubectl --kubeconfig=/dev/null \
  --server=https://127.0.0.1:8443 \
  --insecure-skip-tls-verify \
  get widget test-widget -o yaml
```

## Project Structure

```
cmd/example-generic-control-plane/
├── main.go                      # Entry point
├── main_test.go                 # Smoke tests
├── go.mod                       # Sub-module
├── go.sum
├── README.md
│
└── pkg/
    ├── admission/
    │   └── kausality.go         # Kausality admission plugin adapter
    │
    ├── apis/example/
    │   ├── doc.go
    │   ├── register.go
    │   └── v1alpha1/
    │       ├── doc.go
    │       ├── register.go
    │       ├── types.go         # Widget, WidgetSet types
    │       └── zz_generated.deepcopy.go
    │
    ├── apiserver/
    │   └── apiserver.go         # Server config and setup
    │
    └── registry/example/
        ├── widget/
        │   └── strategy.go      # Widget storage strategy
        └── widgetset/
            └── strategy.go      # WidgetSet storage strategy
```

## Sub-module

This example is a separate Go module to avoid pulling embeddedetcd dependencies into the main kausality module. The go.mod uses a replace directive to reference the local kausality code:

```go
replace github.com/kausality-io/kausality => ../..
```

## References

- [kcp-dev/generic-controlplane](https://github.com/kcp-dev/generic-controlplane) - Complete generic control plane
- [kubernetes/sample-apiserver](https://github.com/kubernetes/sample-apiserver) - Kubernetes sample apiserver
- [k8s.io/apiserver](https://pkg.go.dev/k8s.io/apiserver) - Generic API server library
