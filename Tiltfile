# Tiltfile for kausality local development
# Run with: tilt up

# Use ko for building Go images
load('ext://ko', 'ko_build')

# Configuration
k8s_namespace = 'kausality-system'

# Build the webhook image with ko
ko_build(
    'kausality-webhook',
    './cmd/kausality-webhook',
    deps=['./cmd/kausality-webhook', './pkg'],
)

# Build the controller image with ko
ko_build(
    'kausality-controller',
    './cmd/kausality-controller',
    deps=['./cmd/kausality-controller', './pkg'],
)

# Build the backend-log image with ko
ko_build(
    'kausality-backend-log',
    './cmd/kausality-backend-log',
    deps=['./cmd/kausality-backend-log', './pkg'],
)

# Build the backend-tui image with ko
ko_build(
    'kausality-backend-tui',
    './cmd/kausality-backend-tui',
    deps=['./cmd/kausality-backend-tui', './pkg'],
)

# Deploy using Helm
k8s_yaml(helm(
    './charts/kausality',
    name='kausality',
    namespace=k8s_namespace,
    values=['./tilt-values.yaml'],
    set=[
        'image.repository=kausality-webhook',
        'image.tag=tilt',
        'image.pullPolicy=Never',
        'controller.image.repository=kausality-controller',
        'controller.image.tag=tilt',
        'controller.image.pullPolicy=Never',
        'backend.image.repository=kausality-backend-log',
        'backend.image.tag=tilt',
        'backend.image.pullPolicy=Never',
        'backendTui.image.repository=kausality-backend-tui',
        'backendTui.image.tag=tilt',
        'backendTui.image.pullPolicy=Never',
    ],
))

# Create the namespace if it doesn't exist
local_resource(
    'create-namespace',
    'kubectl create namespace %s --dry-run=client -o yaml | kubectl apply -f -' % k8s_namespace,
    deps=[],
)

# Configure resource dependencies
k8s_resource(
    'kausality-webhook',
    resource_deps=['create-namespace'],
    port_forwards=['8081:8081'],  # health endpoint
    labels=['webhook'],
)

k8s_resource(
    'kausality-controller',
    resource_deps=['create-namespace'],
    labels=['controller'],
)

k8s_resource(
    'kausality-backend-log',
    resource_deps=['create-namespace', 'kausality-webhook'],
    labels=['backend'],
)

k8s_resource(
    'kausality-backend-tui',
    resource_deps=['create-namespace', 'kausality-webhook'],
    labels=['backend'],
)
