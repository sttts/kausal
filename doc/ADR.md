# Architecture Decision Record

## Context

Controllers reconcile infrastructure by applying changes to downstream Kubernetes objects and Terraform. Currently, there is no mechanism to distinguish between:
- **Expected changes**: Triggered by spec changes (generation != observedGeneration)
- **Unexpected changes**: Triggered by drift, external modifications, or software updates when generation == observedGeneration

Unexpected changes can cause infrastructure modifications without explicit intent, creating operational risk. Examples include:
- External modifications to cloud resources (drift)
- Updates to referenced resources (ClusterRelease, MachineClass)
- Controller behavior changes from software updates

## Decision

Implement an **admission-only** change detection and approval system that:
1. Detects unexpected changes by checking parent state at admission time
2. Blocks unexpected changes until explicitly approved or matching an exception policy
3. Escalates to Slack for human (or AI) approval
4. Tracks request provenance through the resource hierarchy

**Key constraint**: No controller modifications required. All logic runs in admission.

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
