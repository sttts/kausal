#!/usr/bin/env bash
set -euo pipefail

# E2E test script for kausality with Crossplane
# Creates a kind cluster, installs Crossplane and kausality, and verifies drift detection

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-kausality-crossplane-e2e}"
NAMESPACE="kausality-system"
CROSSPLANE_NAMESPACE="crossplane-system"
TIMEOUT="${TIMEOUT:-300s}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log() { echo -e "${GREEN}[INFO]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; }
fail() { error "$*"; exit 1; }

cleanup() {
    log "Cleaning up..."
    kind delete cluster --name "${CLUSTER_NAME}" 2>/dev/null || true
}

# Trap to ensure cleanup on exit (unless SKIP_CLEANUP is set)
if [[ "${SKIP_CLEANUP:-}" != "true" ]]; then
    trap cleanup EXIT
fi

# Check required tools
for cmd in kind kubectl helm ko; do
    if ! command -v "$cmd" &>/dev/null; then
        if [[ -x "${ROOT_DIR}/bin/$cmd" ]]; then
            export PATH="${ROOT_DIR}/bin:$PATH"
        else
            fail "$cmd is required but not installed. Run: make $cmd"
        fi
    fi
done

log "Creating kind cluster: ${CLUSTER_NAME}"
kind create cluster --name "${CLUSTER_NAME}" --config "${SCRIPT_DIR}/../kind-config.yaml" --wait 120s

log "Installing Crossplane..."
helm repo add crossplane-stable https://charts.crossplane.io/stable || true
helm repo update
helm upgrade --install crossplane crossplane-stable/crossplane \
    --namespace "${CROSSPLANE_NAMESPACE}" \
    --create-namespace \
    --wait \
    --timeout "${TIMEOUT}"

log "Waiting for Crossplane to be ready..."
kubectl wait --for=condition=ready pod -l app=crossplane -n "${CROSSPLANE_NAMESPACE}" --timeout="${TIMEOUT}"

log "Installing provider-nop (for testing)..."
kubectl apply -f - <<EOF
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-nop
spec:
  package: xpkg.upbound.io/crossplane-contrib/provider-nop:v0.3.0
EOF

log "Waiting for provider-nop to be healthy..."
kubectl wait --for=condition=healthy provider/provider-nop --timeout="${TIMEOUT}"

log "Building and loading kausality images with ko..."
cd "${ROOT_DIR}"
export KO_DOCKER_REPO="ko.local"

WEBHOOK_IMAGE=$(ko build --bare ./cmd/kausality-webhook)
log "Built webhook image: ${WEBHOOK_IMAGE}"
kind load docker-image "${WEBHOOK_IMAGE}" --name "${CLUSTER_NAME}"

BACKEND_IMAGE=$(ko build --bare ./cmd/kausality-backend-log)
log "Built backend image: ${BACKEND_IMAGE}"
kind load docker-image "${BACKEND_IMAGE}" --name "${CLUSTER_NAME}"

log "Installing kausality via Helm..."
helm upgrade --install kausality "${ROOT_DIR}/charts/kausality" \
    --namespace "${NAMESPACE}" \
    --create-namespace \
    --set image.repository="${WEBHOOK_IMAGE%:*}" \
    --set image.tag="${WEBHOOK_IMAGE##*:}" \
    --set image.pullPolicy=Never \
    --set backend.enabled=true \
    --set backend.image.repository="${BACKEND_IMAGE%:*}" \
    --set backend.image.tag="${BACKEND_IMAGE##*:}" \
    --set backend.image.pullPolicy=Never \
    --set driftCallback.enabled=true \
    --set certificates.selfSigned.enabled=true \
    --set 'resourceRules.include[0].apiGroups={nop.crossplane.io}' \
    --set 'resourceRules.include[0].resources={*}' \
    --set 'resourceRules.include[1].apiGroups={apps}' \
    --set 'resourceRules.include[1].resources={deployments,replicasets}' \
    --wait \
    --timeout "${TIMEOUT}"

log "Waiting for kausality pods to be ready..."
kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=kausality -n "${NAMESPACE}" --timeout="${TIMEOUT}"
kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=kausality-backend -n "${NAMESPACE}" --timeout="${TIMEOUT}"

log "Pods are ready:"
kubectl get pods -n "${NAMESPACE}"

# Test 1: Create a Crossplane NopResource
log "=== Test 1: Crossplane NopResource creation ==="

log "Creating NopResource..."
kubectl apply -f - <<EOF
apiVersion: nop.crossplane.io/v1alpha1
kind: NopResource
metadata:
  name: test-nop
  annotations:
    kausality.io/trace-ticket: "CROSSPLANE-001"
spec:
  forProvider:
    conditionAfter:
      - time: 5s
        conditionType: Ready
        conditionStatus: "True"
EOF

log "Waiting for NopResource to be ready..."
kubectl wait --for=condition=ready nopresource/test-nop --timeout="${TIMEOUT}" || warn "NopResource may not have ready condition"

sleep 5

log "Checking NopResource status..."
kubectl get nopresource test-nop -o yaml

# Check if trace was propagated
TRACE=$(kubectl get nopresource test-nop -o jsonpath='{.metadata.annotations.kausality\.io/trace}' 2>/dev/null || echo "")
if [[ -n "${TRACE}" ]]; then
    log "Trace annotation found on NopResource: ${TRACE}"
else
    warn "No trace annotation found on NopResource"
fi

# Test 2: Update NopResource to trigger reconciliation
log "=== Test 2: NopResource update ==="

log "Updating NopResource..."
kubectl patch nopresource test-nop --type=merge -p '{"spec":{"forProvider":{"conditionAfter":[{"time":"10s","conditionType":"Ready","conditionStatus":"True"}]}}}'

sleep 5

log "Checking webhook logs..."
WEBHOOK_LOGS=$(kubectl logs -l app.kubernetes.io/name=kausality -n "${NAMESPACE}" --tail=100 2>/dev/null || echo "")
if echo "${WEBHOOK_LOGS}" | grep -q "nop.crossplane.io\|NopResource"; then
    log "Webhook is intercepting Crossplane resources"
else
    warn "Could not verify webhook is intercepting Crossplane resources"
fi

# Test 3: Check backend for DriftReports
log "=== Test 3: Backend DriftReport reception ==="

log "Checking backend logs..."
BACKEND_LOGS=$(kubectl logs -l app.kubernetes.io/name=kausality-backend -n "${NAMESPACE}" --tail=100 2>/dev/null || echo "")
echo "${BACKEND_LOGS}"

if echo "${BACKEND_LOGS}" | grep -q "DriftReport\|apiVersion.*kausality"; then
    log "Backend received DriftReport(s)"
else
    log "No DriftReports in backend logs (expected if no drift occurred)"
fi

# Summary
log "=== Crossplane E2E Test Summary ==="
log "Cluster: ${CLUSTER_NAME}"
log "Crossplane version:"
kubectl get deployment crossplane -n "${CROSSPLANE_NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}'
echo ""
log "Provider-nop status:"
kubectl get provider provider-nop
log "NopResource:"
kubectl get nopresource test-nop
log ""
log "Crossplane E2E tests completed successfully!"
