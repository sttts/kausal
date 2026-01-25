#!/usr/bin/env bash
set -euo pipefail

# Install third-party dependencies for Crossplane E2E tests.
# This installs Crossplane, provider-nop, and function-patch-and-transform.
# Kausality installation is handled by Makefile.

CROSSPLANE_NAMESPACE="crossplane-system"
TIMEOUT="${TIMEOUT:-300s}"

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

log() { echo -e "${GREEN}[INFO]${NC} $*"; }
fail() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# Check required tools
for cmd in kubectl helm; do
    command -v "$cmd" &>/dev/null || fail "$cmd is required but not installed"
done

log "=========================================="
log "Installing Crossplane dependencies"
log "=========================================="

# Install Crossplane
log "Installing Crossplane..."
helm repo add crossplane-stable https://charts.crossplane.io/stable 2>/dev/null || true
helm repo update
helm upgrade --install crossplane crossplane-stable/crossplane \
    --namespace "${CROSSPLANE_NAMESPACE}" \
    --create-namespace \
    --wait \
    --timeout "${TIMEOUT}"

log "Waiting for Crossplane to be ready..."
kubectl wait --for=condition=ready pod -l app=crossplane -n "${CROSSPLANE_NAMESPACE}" --timeout="${TIMEOUT}"

# Install provider-nop
log "Installing provider-nop..."
kubectl apply -f - <<EOF
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-nop
spec:
  package: xpkg.upbound.io/crossplane-contrib/provider-nop:v0.3.0
EOF

log "Waiting for provider-nop to be healthy..."
until kubectl get provider provider-nop -o jsonpath='{.status.conditions[?(@.type=="Healthy")].status}' 2>/dev/null | grep -q True; do
    sleep 2
done

# Install function-patch-and-transform
log "Installing function-patch-and-transform..."
kubectl apply -f - <<EOF
apiVersion: pkg.crossplane.io/v1beta1
kind: Function
metadata:
  name: function-patch-and-transform
spec:
  package: xpkg.upbound.io/crossplane-contrib/function-patch-and-transform:v0.7.0
EOF

log "Waiting for function-patch-and-transform to be healthy..."
until kubectl get function function-patch-and-transform -o jsonpath='{.status.conditions[?(@.type=="Healthy")].status}' 2>/dev/null | grep -q True; do
    sleep 2
done

log ""
log "Crossplane dependencies installed:"
kubectl get pods -n "${CROSSPLANE_NAMESPACE}"
kubectl get providers
kubectl get functions
log ""
log "=========================================="
log "Crossplane dependencies ready"
log "=========================================="
