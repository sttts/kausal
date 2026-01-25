# Drift Notification Webhooks

## Overview

When drift is detected, kausality sends a `DriftReport` to configured webhook endpoints. Webhooks handle notifications (Slack, CLI, etc.) and apply actions via the Kubernetes API.

**API Style**: Similar to admission webhooks — POST endpoint receiving `kind: DriftReport`

**Configuration**: Flags (`--drift-webhook-url`, `--drift-webhook-timeout`, etc.)

**Deduplication**: Content-based ID hash; only send once per unique drift occurrence.

**Resolution**: Send `phase: Resolved` when drift is resolved (parent spec changed, approval added, or child deleted).

## DriftReport (kausality.io/v1alpha1)

```yaml
apiVersion: kausality.io/v1alpha1
kind: DriftReport
spec:
  id: "a1b2c3d4e5f67890"  # sha256(parent+child+diff)[:16]
  phase: Detected         # or Resolved
  parent:
    apiVersion: example.com/v1alpha1
    kind: EKSCluster
    namespace: infra
    name: prod
    generation: 5
    observedGeneration: 5
    controllerManager: "eks-controller"
    lifecyclePhase: "Initialized"
  child:
    apiVersion: v1
    kind: ConfigMap
    namespace: infra
    name: cluster-config
    uid: "abc-123-def"
    generation: 3
  oldObject: { ... }      # Previous state (UPDATE only, optional)
  newObject: { ... }      # Current state (required)
  request:
    user: "system:serviceaccount:infra:eks-controller"
    groups:
      - "system:serviceaccounts"
      - "system:serviceaccounts:infra"
    uid: "abc-123"
    fieldManager: "eks-controller"
    operation: "UPDATE"
    dryRun: false
```

**Key design decisions:**
- No `ObjectMeta` — transient type with no persistence, only `TypeMeta` for API identification
- Parent includes `observedGeneration`, `controllerManager`, `lifecyclePhase` — all detection context in one place
- Uses `runtime.RawExtension` for embedded objects (standard Kubernetes type)
- `newObject` is required, `oldObject` is optional (only for UPDATE)

## Resolution Triggers

Send `phase: Resolved` when:
1. Parent spec changed (generation incremented) — no longer drift
2. Approval annotation added for this child
3. Child object deleted

## Action Implementations

Webhook implementations apply actions via Kubernetes API:

| Action | Annotation Change |
|--------|------------------|
| Approve (once) | Add `kausality.io/approvals` with `mode: once` |
| Approve (generation) | Add `kausality.io/approvals` with `mode: generation` |
| Ignore (always) | Add `kausality.io/approvals` with `mode: always` |
| Freeze | Set `kausality.io/freeze` with `{"user":..., "message":..., "at":...}` |
| Snooze | Set `kausality.io/snooze` with `{"expiry":..., "user":..., "message":...}` |

## Slack Escalation

When unexpected change detected and no approval/policy match:
1. Post to Slack channel with:
   - Object reference (kind, namespace, name)
   - Diff of old vs new object
   - Request trace for context
   - Buttons: Approve, Reject, Add Exception
2. "Add Exception" opens dialog to create ApprovalPolicy rule
3. Approval/rejection updates the annotation via Slack bot
