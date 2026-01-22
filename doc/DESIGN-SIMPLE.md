# Drift Detection and Escalation

**Simplified Design**

## Context

Controllers reconcile infrastructure by applying changes to downstream Kubernetes objects and Terraform. Currently, there is no mechanism to distinguish between:
- **Expected changes**: Triggered by spec changes (generation != observedGeneration)
- **Unexpected changes**: Triggered by drift, external modifications, or software updates when generation == observedGeneration

Unexpected changes can cause infrastructure modifications without explicit intent, creating operational risk. Examples include:
- External modifications to cloud resources (drift)
- Updates to referenced resources (ClusterRelease, MachineClass)
- Controller behavior changes from software updates

## Design

Implement a change detection and approval system that:
1. Detects unexpected changes via dry-run before applying
2. Blocks unexpected changes until explicitly approved or matching an exception policy
3. Escalates to Slack for human (or AI) approval
4. Tracks request provenance through the resource hierarchy

### Detection Mechanism

**For Kubernetes Objects (Server-Side Apply):**
- Use SSA dry-run to detect if an apply would modify the object
- Compare dry-run result against current state
- Only proceed if no changes OR change is approved

**For Terraform (L0 controllers):**
- Check if plan is non-empty when generation == observedGeneration
- Future: TerraformApprovalPolicy CRD for plan-based exceptions

### Approval and Rejection Annotations

```yaml
# On parent resource - approvals for allowed changes
kausality.io/approvals: |
  [
    {
      "apiVersion": "v1",
      "kind": "ConfigMap",
      "name": "bar",
      "generation": 5,
      "expiry": "2026-01-21T12:00:00Z"
    }
  ]

# On parent resource - rejections for blocked changes
kausality.io/rejections: |
  [
    {
      "apiVersion": "example.com/v1alpha1",
      "kind": "EKSCluster",
      "name": "cluster-1",
      "generation": 2,
      "reason": "Change not approved by SRE team"
    }
  ]
```

- Namespace is implicit (same as parent) - only applies to namespaced resources
- Approvals are consumed (removed) when used
- Rejections remain and cause `Synced=False` with rejection message
- Admission plugin prunes both lists when parent generation changes

### ApprovalPolicy CRD

```yaml
apiVersion: kausality.io/v1alpha1
kind: ApprovalPolicy
metadata:
  name: controller-l1-compute  # service account name
  namespace: infra
spec:
  rules:
  - apiVersion: "v1"
    kind: "ConfigMap"
    namespace: "*"
    name: "*"
    prereqs:  # CEL/regexp on old object
      cel: "object.metadata.labels['env'] == 'dev'"
    match:    # CEL/regexp on new object
      regexp: '{"data":{"version":".*"}}'
    expiry: "2026-06-01T00:00:00Z"
```

- Namespace-scoped in controller namespace
- Named by controller's service account
- Supports wildcards for namespace/name
- Future: ClusterApprovalPolicy for cluster-wide rules

### Request Tracing

```yaml
# Initialized by admission at L2/L1, propagated down
kausality.io/request-trace: "ace-123,cluster-456,ekscluster-789"
kausality.io/jira-issue: "NGCC-1234"  # optional, passed through
```

- Auto-generated at admission if not present
- Appended at each controller level
- Auxiliary fields (jira-issue) passed through unchanged

### Slack Escalation

When unexpected change detected and no approval/policy match:
1. Post to Slack channel with:
   - Object reference (kind, namespace, name)
   - Diff of old vs new object
   - Request trace for context
   - Buttons: Approve, Reject, Add Exception
2. "Add Exception" opens dialog to create ApprovalPolicy rule
3. Approval/rejection updates the annotation via Slack bot

### Reconcile Flow

```
if generation != observedGeneration:
    proceed with changes (expected)
else if not Initialized:
    proceed with changes (initial setup)
else:
    dry-run the change
    if no diff:
        proceed (no-op)
    else:
        check rejections annotation
        if rejected:
            set Synced=False with rejection message, stop
        check approvals annotation
        if approved:
            consume approval, proceed
        check ApprovalPolicy rules
        if policy matches:
            proceed
        else:
            escalate to Slack, block reconcile
```

## Implementation

### Phase 1: SSA Dry-Run Change Detection (Logging Only)
- Implement dry-run detection in SSA apply helpers
- Log when unexpected changes would occur
- Optional Slack notifications (token and client already available)
- No blocking, observability only

### Phase 2: Request-Trace Annotation
- Add admission plugin to initialize request-trace
- Propagate trace through controller hierarchy

### Phase 3: Per-Object Approval Annotation
- Implement approval annotation parsing
- Add pruning on generation change via admission
- Block unexpected changes without approval

### Phase 4: ApprovalPolicy CRD and Slack Integration
- Define and implement ApprovalPolicy CRD
- Integrate Slack escalation workflow
- Implement approval/rejection via Slack bot

### Phase 5: TerraformApprovalPolicy for L0 Controllers
- Extend to Terraform plan-based detection
- Add TerraformApprovalPolicy CRD

## Rationale

- **Explicit Control**: Provides explicit control over unexpected infrastructure changes rather than silent modifications
- **Audit Trail**: Request-trace and Slack history provide comprehensive audit trail
- **Flexible Exceptions**: ApprovalPolicy rules reduce operational toil for known-safe patterns
- **AI-Ready**: Foundation for AI-assisted approvals in the future

## Consequences

### Positive
- Explicit control over unexpected infrastructure changes
- Audit trail via request-trace and Slack history
- Flexible exception rules reduce operational toil
- Foundation for AI-assisted approvals

### Negative
- Reconcile latency when approval required
- Operational overhead for initial policy setup
- Slack dependency for escalation path

### Mitigations
- Phase 1 logging-only allows gradual rollout
- ApprovalPolicy reduces manual approval burden
- Expiry on approvals prevents stale exceptions

## Alternatives Considered

### Metrics-only Alerting (No Blocking)
Rejected: Does not prevent unexpected changes, only detects after the fact.

### Per-Resource Approval Annotations Only (No Policies)
Rejected: Would require manual approval for every change, too much operational burden.

### Hash-Based Spec Change Detection Instead of Generation
Rejected: Generation/observedGeneration is already the standard Kubernetes pattern and sufficient for our needs.
