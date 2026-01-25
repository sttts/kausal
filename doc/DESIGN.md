# Drift Detection and Escalation

**Simplified Design — Admission-Only**

## Context

Controllers reconcile infrastructure by applying changes to downstream Kubernetes objects and Terraform. Currently, there is no mechanism to distinguish between:
- **Expected changes**: Triggered by spec changes (generation != observedGeneration)
- **Unexpected changes**: Triggered by drift, external modifications, or software updates when generation == observedGeneration

Unexpected changes can cause infrastructure modifications without explicit intent, creating operational risk. Examples include:
- External modifications to cloud resources (drift)
- Updates to referenced resources (ClusterRelease, MachineClass)
- Controller behavior changes from software updates

## Design

Implement an **admission-only** change detection and approval system that:
1. Detects unexpected changes by checking parent state at admission time
2. Blocks unexpected changes until explicitly approved or matching an exception policy
3. Escalates to Slack for human (or AI) approval
4. Tracks request provenance through the resource hierarchy

**Key constraint**: No controller modifications required. All logic runs in admission.

### Detection Mechanism

When a mutation is intercepted, admission:

1. **Identifies the actor** by comparing `request.options.fieldManager` with the parent's controller manager
2. **Checks the parent's state** (`generation` vs `observedGeneration`)

```
parent := resolve via controller ownerReference (controller: true)
isController := request.fieldManager == parent.managedFields[observedGeneration].manager

if NOT isController:
    # Different actor → new causal origin (not drift)
    # Start new trace, may require approval via ApprovalPolicy
else if parent.generation != parent.status.observedGeneration:
    # Controller is reconciling → expected change → ALLOW
else:
    # Controller updating but parent unchanged → DRIFT
    # Check approvals
```

| Actor | Parent State | Result |
|-------|--------------|--------|
| Controller | gen != obsGen | **Expected** — controller is reconciling |
| Controller | gen == obsGen | **Drift** — controller changing without spec change |
| Different actor | any | **New origin** — not drift, requires ApprovalPolicy |

**Drift** specifically means: the controller is making changes when the parent spec hasn't changed (gen == obsGen). This indicates unexpected reconciliation triggered by external factors (drift in cloud state, software updates, etc.).

**Different actors** (kubectl, HPA, GitOps tools) are not considered drift — they're simply different causal chains. These may still require approval via ApprovalPolicy, but they're semantically different from controller drift.

**Spec changes only**: Kausality only intercepts mutations to `spec`. Changes to `status` or `metadata` are ignored — no drift detection, no tracing, no approval required. Status updates are controllers reporting state, and metadata changes are typically administrative.

### Controller Identification

A key challenge is identifying whether a mutation comes from the controller (expected) or another actor (potential drift). We use Kubernetes managedFields for this.

**The controller is identified by who updates `status.observedGeneration` on the parent.**

This is reliable because:
- Only the reconciling controller updates `observedGeneration`
- It's tracked in `parent.metadata.managedFields`
- The manager string stays in sync across upgrades (controller updates both parent status and children)

**Algorithm:**

```
1. Request comes in for child object
2. Find parent via controller ownerReference (controller: true)
3. Look at parent's managedFields → find manager of f:status.f:observedGeneration
4. Compare request.options.fieldManager with that manager
5. If match → controller is updating → check parent gen vs obsGen for drift
6. If no match → different actor → new trace origin (potential drift)
```

**Example managedFields:**
```yaml
managedFields:
- manager: capi-controller
  operation: Update
  subresource: status
  fieldsV1:
    f:status:
      f:observedGeneration: {}
      f:conditions: {}
```

Here, `capi-controller` is the controller. Any child update with `fieldManager: capi-controller` is from the controller.

**Why not use userInfo.username?**
- `fieldManager` is available on all mutating requests (Create, Update, Patch)
- Manager strings stay consistent within a controller's operation
- No annotation needed — fully dynamic comparison against existing managedFields

**Late installation:** Works automatically. The parent's managedFields already contains the controller's manager string from previous status updates.

**Non-owning controllers (HPA, VPA):** These don't set controller ownerReferences and don't update `observedGeneration`. They appear as different actors and create new trace origins. This is NOT drift — it's simply a different causal chain. Use ApprovalPolicy to allow known actors like HPA.

**Implementation:** Use `sigs.k8s.io/structured-merge-diff` for parsing managedFields. This is the official library used by the Kubernetes API server.

**For Terraform (L0 controllers)**:
- Check if plan is non-empty when generation == observedGeneration
- Use drift notification webhooks for plan review workflows

### Approval and Rejection Annotations

```yaml
# On parent resource (e.g., EKSCluster)
metadata:
  annotations:
    # Condensed JSON format (single line recommended)
    kausality.io/approvals: '[{"apiVersion":"v1","kind":"ConfigMap","name":"bar","generation":5,"mode":"once"},{"apiVersion":"v1","kind":"Secret","name":"credentials","mode":"always"}]'
    kausality.io/rejections: '[{"apiVersion":"example.com/v1alpha1","kind":"NodePool","name":"pool-1","generation":5,"reason":"Destructive change, needs SRE review"}]'
    kausality.io/trace: '[{"apiVersion":"example.com/v1alpha1","kind":"EKSCluster","name":"prod","generation":5,"user":"hans@example.com","timestamp":"2026-01-24T10:30:00Z"}]'
```

**Approval fields:**
- `apiVersion`, `kind`, `name`: Child resource reference (required)
- `generation`: Parent generation this approval is valid for (required for `once`/`generation` modes)
- `mode`: One of `once`, `generation`, `always` (defaults to `once`)

**Rejection fields:**
- `apiVersion`, `kind`, `name`: Child resource reference (required)
- `generation`: Parent generation this rejection applies to (optional; if omitted, always active)
- `reason`: Human-readable explanation (required)

- Namespace is implicit (same as parent) — only applies to namespaced resources
- `generation` field is only required for `once` and `generation` modes, not for `always`
- Admission plugin prunes approvals when parent generation changes

### Approval Modes

| Mode | Behavior | Use Case |
|------|----------|----------|
| `once` | Removed after first allowed mutation | One-time drift fix, strict control |
| `generation` | Valid while `parent.generation == approval.generation` | Approve for current state, invalidate on spec change |
| `always` | Permanent, never automatically pruned | Known-safe pattern, permanent exception |

### Rejection Priority

**Rejections are checked before approvals.** If a child has both an approval and a rejection, the rejection wins. This ensures explicit blocks cannot be accidentally bypassed.

### Approval Validity

An approval is valid when:
1. No matching rejection exists for this child
2. `approval.apiVersion/kind/name` matches the child being mutated
3. Mode-specific:
   - `once`: not yet consumed AND `approval.generation == parent.generation`
   - `generation`: `approval.generation == parent.generation`
   - `always`: always valid

### Pruning Rules

| Trigger | Effect |
|---------|--------|
| Parent generation changes | `once` and `generation` approvals with `generation < parent.generation` are pruned |
| Approval used (`mode: once`) | That specific approval is removed |
| `mode: always` | Never pruned automatically (explicit removal required) |

### Enforcement Mode

The `kausality.io/mode` annotation controls whether drift is logged or enforced:

```yaml
metadata:
  annotations:
    kausality.io/mode: "enforce"  # or "log"
```

| Value | Behavior |
|-------|----------|
| `log` | Drift is detected and logged, warnings returned, but mutations are allowed |
| `enforce` | Drift without approval is denied with an error message |

**Precedence (most specific wins):**
1. Object annotation `kausality.io/mode` on the child being mutated
2. Namespace annotation `kausality.io/mode` on the child's namespace
3. Global default from webhook configuration (`--default-mode` flag / Helm `defaultMode`)

**Example: Enforce mode on a namespace**

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: production
  annotations:
    kausality.io/mode: "enforce"
```

All drift in the `production` namespace will be blocked unless approved.

**Example: Log mode override on specific object**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: experimental
  namespace: production
  annotations:
    kausality.io/mode: "log"  # Override namespace's enforce mode
```

This deployment allows drift (with warnings) even though the namespace has enforce mode.

### Freeze and Snooze

Additional parent annotations for operational control:

```yaml
metadata:
  annotations:
    kausality.io/freeze: "true"                        # Block ALL changes
    kausality.io/snooze-until: "2026-01-25T00:00:00Z"  # Suppress escalation
```

| Annotation | Effect |
|------------|--------|
| `freeze: "true"` | Block ALL child mutations, even expected changes (parent spec change). Emergency lockdown. |
| `snooze-until: <timestamp>` | Block drift but don't escalate to Slack until timestamp. Suppresses notifications, not permissions. |

### Lifecycle Phases

#### Initialization

During initialization, all child changes are allowed (including CREATE). Detection priority (default, configurable per GVK):

1. `Initialized=True` condition exists
2. `Ready=True` condition exists (with persistence — see below)
3. `status.observedGeneration` exists

Once initialized, admission stores:
```yaml
kausality.io/initialized: "true"
```

This persists the initialized state for resources with flapping conditions (e.g., Crossplane Ready).

#### Deletion

When parent has `metadata.deletionTimestamp`:
- Allow ALL child mutations (cleanup phase)
- No drift checks, no approvals needed

### Operations by Type

| Operation | Drift Rules |
|-----------|-------------|
| CREATE | Allowed during initialization. Blocked during drift (requires approval). |
| UPDATE | Blocked during drift unless approved. |
| DELETE | Blocked during drift unless approved (same as UPDATE). |

### ApprovalPolicy CRD

For pattern-based exceptions (reduce per-object approval burden):

```yaml
apiVersion: kausality.io/v1alpha1
kind: ApprovalPolicy
metadata:
  name: controller-l1-compute   # typically matches SA name
  namespace: infra
spec:
  rules:
  - apiVersion: "v1"
    kind: "ConfigMap"
    namespace: "*"
    name: "*"
    prereqs:                    # CEL on old object
      cel: "object.metadata.labels['env'] == 'dev'"
    match:                      # CEL/regexp on new object
      regexp: '{"data":{"version":".*"}}'
    mode: "always"
    expiry: "2026-06-01T00:00:00Z"
```

- Namespace-scoped in controller namespace
- Named by controller's service account
- Supports wildcards for namespace/name
- Future: ClusterApprovalPolicy for cluster-wide rules

### Request Tracing

The trace is a JSON array stored in `kausality.io/trace`, recording the causal chain of mutations:

```yaml
kausality.io/trace: |
  [
    {
      "apiVersion": "example.com/v1alpha1",
      "kind": "EKSCluster",
      "name": "prod-cluster",
      "generation": 5,
      "user": "hans@example.com",
      "timestamp": "2026-01-24T10:30:00Z"
    },
    {
      "apiVersion": "example.com/v1alpha1",
      "kind": "NodePool",
      "name": "pool-1",
      "generation": 12,
      "user": "system:serviceaccount:infra:node-controller",
      "timestamp": "2026-01-24T10:30:05Z"
    }
  ]
```

Each entry contains:
- Resource reference (apiVersion, kind, name)
- `generation` at mutation time
- `user` from admission (human/CI at origin, service account for controllers)
- `timestamp`

Namespace is omitted — it's the same as the object carrying the trace (or cluster-scoped).

#### Origin vs Controller Hop

**Origin (new trace):**
- No controller ownerReference, OR
- Parent has `generation == observedGeneration` (no active reconciliation), OR
- `request.fieldManager` does not match parent's `observedGeneration` manager (different actor)
- → Start new trace, this user is the initiator

**Controller hop (extend trace):**
- Has controller ownerReference with `controller: true` AND
- Parent has `generation != observedGeneration` (parent is reconciling) AND
- `request.fieldManager` matches manager of parent's `status.observedGeneration` in managedFields
- → Copy trace from parent, append new hop

GitOps tools (ArgoCD, Flux) appear as **origins** since they apply manifests directly without `controller: true` ownerReferences. Kubernetes controllers (Deployment→ReplicaSet→Pod) appear as **hops**.

Non-owning controllers like HPA also appear as **origins** — they update objects without ownerReferences and don't match the primary controller's manager. Use ApprovalPolicy to allow known actors.

#### Trace Lifecycle

- **Created** when a mutation has no parent trace to extend
- **Extended** when a controller propagates changes to children
- **Replaced** when parent generation changes (new causal chain starts)

#### Trace Labels

Custom metadata can be attached to trace hops via `kausality.io/trace-*` annotations:

```yaml
metadata:
  annotations:
    kausality.io/trace-ticket: "JIRA-123"
    kausality.io/trace-pr: "567"
    kausality.io/trace-deployment: "deploy-42"
```

These become `labels` in the trace hop:

```json
{
  "apiVersion": "apps/v1",
  "kind": "Deployment",
  "name": "prod",
  "generation": 5,
  "user": "hans@example.com",
  "timestamp": "2026-01-24T10:30:00Z",
  "labels": {"ticket": "JIRA-123", "pr": "567", "deployment": "deploy-42"}
}
```

Each hop captures labels from its own object's annotations. Labels are not inherited from parent to child — the parent's labels are already visible in the parent's hop entry.

### Drift Notification Webhook

When drift is detected, kausality sends a `DriftReport` to configured webhook endpoints. Webhooks handle notifications (Slack, CLI, etc.) and apply actions via the Kubernetes API.

**API Style**: Similar to admission webhooks — POST endpoint receiving `kind: DriftReport`

**Configuration**: Flags (`--drift-webhook-url`, `--drift-webhook-timeout`, etc.)

**Deduplication**: Content-based ID hash; only send once per unique drift occurrence.

**Resolution**: Send `phase: Resolved` when drift is resolved (parent spec changed, approval added, or child deleted).

#### DriftReport (kausality.io/v1alpha1)

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

#### Resolution Triggers

Send `phase: Resolved` when:
1. Parent spec changed (generation incremented) — no longer drift
2. Approval annotation added for this child
3. Child object deleted

#### Action Implementations

Webhook implementations apply actions via Kubernetes API:

| Action | Annotation Change |
|--------|------------------|
| Approve (once) | Add `kausality.io/approvals` with `mode: once` |
| Approve (generation) | Add `kausality.io/approvals` with `mode: generation` |
| Ignore (always) | Add `kausality.io/approvals` with `mode: always` |
| Freeze | Add `kausality.io/rejections` entry |
| Snooze | Set `kausality.io/snooze-until: <timestamp>` |

### Slack Escalation

When unexpected change detected and no approval/policy match:
1. Post to Slack channel with:
   - Object reference (kind, namespace, name)
   - Diff of old vs new object
   - Request trace for context
   - Buttons: Approve, Reject, Add Exception
2. "Add Exception" opens dialog to create ApprovalPolicy rule
3. Approval/rejection updates the annotation via Slack bot

### Admission Flow

```
1. Receive child CREATE/UPDATE/DELETE (oldObject, object, userInfo)
2. Resolve parent via controller ownerReference (controller: true)

3. Check lifecycle phases (short-circuit):
   a. If parent has deletionTimestamp → ALLOW (deletion cleanup)
   b. If parent not initialized → ALLOW (initialization phase)
   c. If parent has freeze annotation → DENY (frozen)

4. If parent.generation != parent.status.observedGeneration:
     → Expected change (includes CREATE/UPDATE/DELETE), ALLOW
     → Replace child trace with new chain from parent

5. Else (drift case — applies to CREATE/UPDATE/DELETE):
     a. Check kausality.io/rejections for matching child
        → If rejected: DENY with reason
     b. Check kausality.io/approvals for matching child
        → If approved:
            - Extend trace with drift approval info
            - If mode=once: remove approval, ALLOW
            - If mode=generation: ALLOW (pruned on next gen change)
            - If mode=always: ALLOW
     c. Check ApprovalPolicy rules for pattern match
        → If policy matches: ALLOW
     d. Else:
        - If parent has snooze-until and not expired → DENY (no escalation)
        - Else → DENY and escalate to Slack
```

### Response Codes

| Outcome | Response |
|---------|----------|
| Parent deleting | `allowed: true` |
| Parent initializing | `allowed: true` |
| Parent frozen | `allowed: false`, status 403 Forbidden, reason: frozen |
| Expected change (gen != obsGen) | `allowed: true` |
| Drift with valid approval | `allowed: true` |
| Drift with ApprovalPolicy match | `allowed: true` |
| Drift rejected (explicit rejection) | `allowed: false`, status 403 Forbidden, reason from rejection |
| Drift snoozed (no escalation) | `allowed: false`, status 403 Forbidden, no Slack notification |
| Drift without approval (blocked) | `allowed: false`, status 403 Forbidden, escalate to Slack |
| Parent not found | `allowed: false`, status 422 Unprocessable |

## Deployment Modes

The core logic is implemented as a **Go library** that can be consumed in two ways.

### Library Import (Generic Control Plane)

```go
import "github.com/sttts/kausality/pkg/admission"

// In apiserver setup
admissionHandler := admission.NewHandler(admission.Config{
    Client:        client,
    PolicyLister:  policyInformer.Lister(),
    // ...
})

// Register as admission plugin
server.RegisterAdmission(admissionHandler)
```

- Embedded directly in custom apiserver (k8s.io/apiserver)
- No network latency, no webhook overhead
- ApprovalPolicy CRD served by same apiserver
- Resource targeting is handled by which admission plugins are registered for which resources

### Webhook Server (Stock Kubernetes)

```go
import "github.com/sttts/kausality/pkg/webhook"

// Standalone webhook server
server := webhook.NewServer(webhook.Config{
    Client:       client,
    PolicyLister: policyInformer.Lister(),
    CertDir:      "/etc/webhook/certs",
    // ...
})
server.Run()
```

- Deployed as separate service
- Configured via ValidatingWebhookConfiguration / MutatingWebhookConfiguration
- Helm chart handles webhook registration
- ValidatingAdmissionPolicy (CEL) for simple fast-path checks:
  - `object.metadata.generation == object.status.observedGeneration` → drift candidate
  - `has(object.metadata.deletionTimestamp)` → deletion phase

### Resource Targeting

Which resources are subject to drift detection is **deployment configuration**, not core logic.

#### Configuration Model

```yaml
# For webhook: part of WebhookConfiguration
# For library: passed to admission.Config
resourceRules:
  # Include by API group
  - apiGroups: ["apps"]
    resources: ["*"]

  # Include specific resources
  - apiGroups: ["example.com"]
    resources: ["ekscluster", "nodepools"]

  # Exclude specific resources
  - apiGroups: [""]
    resources: ["configmaps", "secrets"]
    exclude: true
```

#### Webhook Configuration (Helm)

For stock Kubernetes, the Helm chart generates WebhookConfiguration:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: kausality-drift-detection
webhooks:
- name: drift.kausality.io
  rules:
  - apiGroups: ["apps", "example.com"]
    apiVersions: ["*"]
    resources: ["deployments", "ekscluster", "nodepools"]
    operations: ["CREATE", "UPDATE", "DELETE"]
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: NotIn
      values: ["kube-system", "kube-public"]
```

Helm values:
```yaml
resourceRules:
  include:
  - apiGroups: ["apps"]
    resources: ["*"]
  - apiGroups: ["example.com"]
    resources: ["ekscluster", "nodepools"]
  exclude:
  - apiGroups: [""]
    resources: ["configmaps", "secrets"]

excludeNamespaces:
  - kube-system
  - kube-public
```

#### Library Configuration (Generic Control Plane)

For generic control plane, resource targeting is typically hard-coded or loaded from config:

```go
// The library doesn't filter — it processes whatever requests it receives
// Resource targeting is done at the apiserver level (which resources invoke admission)
admission.NewHandler(admission.Config{
    Client:       client,
    PolicyLister: policyInformer.Lister(),
})
```

In a generic control plane, resource targeting is handled by which admission plugins are registered for which resources — not by the admission logic itself.

#### Design Note

Resource targeting is **deployment configuration**, not core library logic:
- **Webhook mode**: Helm chart generates WebhookConfiguration with rules
- **Library mode**: Apiserver registration determines which resources invoke admission

The core admission handler assumes it should process every request it receives. Filtering is external.

## Design Decisions

- **Multi-parent**: Only the controller ownerRef (`controller: true`) is subject to drift detection. Other ownerRefs are ignored.
- **Cross-namespace**: Works as-is. ownerRef traversal works regardless of namespace (cluster-scoped parent, namespaced child is fine).

### Consistency Trade-offs

Admission uses informer cache for parent lookup. Cache may be stale:
- **Stale gen > obsGen**: Cache shows parent not-yet-reconciled, reality is gen==obsGen. Could block expected change.
- **Stale gen == obsGen**: Cache shows drift, reality is gen!=obsGen (spec just changed). Could allow drift without approval.

This is inherent in distributed systems — cross-resource consistency is limited. Mitigation:
- Keep informer cache fresh (reasonable resync period)
- Accept occasional false positives/negatives as acceptable trade-off
- Controllers retry on transient admission errors

## Implementation

### Phase 1: Admission-Based Change Detection (Logging Only) ✓
- Implement admission webhook/plugin
- Check parent generation vs observedGeneration
- Log when drift would be blocked
- No blocking, observability only

### Phase 2: Request-Trace Annotation ✓
- Initialize request-trace on parent mutations
- Propagate/replace trace through controller hierarchy

### Phase 3: Per-Object Approval Annotation ✓
- [x] Implement approval annotation parsing with modes (once, generation, always)
- [x] Implement rejection annotation parsing
- [x] Rejection checking (rejection wins over approval)
- [x] Approval pruning logic (prune stale generations, consume once)
- [x] Integration with admission handler (logging mode)
- [x] Enforce mode (per-G/GR configuration via Helm)
- [x] Approval pruning via admission mutation (update annotations)

### Phase 4: Drift Notification Webhook System ✓
- [x] Implement DriftReport webhook callback (kausality.io/v1alpha1)
- [x] Content-based deduplication (ID hash)
- [x] Send phase=Resolved on drift resolution
- [x] Action helpers for webhook implementations
- [x] Backend implementations (kausality-backend-log, kausality-backend-tui)
- [x] Helm chart integration with backend deployment

### Phase 5: ApprovalPolicy CRD
- Define and implement ApprovalPolicy CRD
- Pattern-based exceptions (reduce per-object approval burden)
- Namespace-scoped policies, ClusterApprovalPolicy for cluster-wide rules

### Phase 6: Slack Integration
- Integrate Slack escalation workflow
- Implement approval/rejection via Slack bot
- "Add Exception" dialog to create ApprovalPolicy rules

## Rationale

- **Explicit Control**: Provides explicit control over unexpected infrastructure changes rather than silent modifications
- **Audit Trail**: Request-trace and Slack history provide comprehensive audit trail
- **Flexible Exceptions**: ApprovalPolicy rules reduce operational toil for known-safe patterns
- **AI-Ready**: Foundation for AI-assisted approvals in the future
- **Controller-Agnostic**: Works without modifying existing controllers

## Consequences

### Positive
- Explicit control over unexpected infrastructure changes
- Audit trail via request-trace and Slack history
- Flexible exception rules reduce operational toil
- Foundation for AI-assisted approvals
- No controller modifications required

### Negative
- Admission latency for parent lookup
- Operational overhead for initial policy setup
- Slack dependency for escalation path
- Informer cache staleness can cause false positives/negatives

### Mitigations
- Phase 1 logging-only allows gradual rollout
- ApprovalPolicy reduces manual approval burden
- Approval modes (once, generation, always) cover different use cases
- Freeze/snooze provide operational escape hatches

## Alternatives Considered

### Metrics-only Alerting (No Blocking)
Rejected: Does not prevent unexpected changes, only detects after the fact.

### Per-Resource Approval Annotations Only (No Policies)
Rejected: Would require manual approval for every change, too much operational burden.

### Hash-Based Spec Change Detection Instead of Generation
Rejected: Generation/observedGeneration is already the standard Kubernetes pattern and sufficient for our needs.

### Controller-Based Detection (Dry-Run in Controller)
Rejected for simplified design: Requires controller modifications. Admission-only approach works without touching controllers.
