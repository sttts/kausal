# Kausality Design

**Admission-based drift detection and approval system for Kubernetes.**

Kausality detects when controllers make unexpected changes to child resources and provides approval workflows to control infrastructure drift.

## Core Concept

**Drift** = controller making changes when parent hasn't changed (`generation == observedGeneration`)

| Actor | Parent State | Result |
|-------|--------------|--------|
| Controller | gen != obsGen | **Expected** — reconciling |
| Controller | gen == obsGen | **Drift** — unexpected change |
| Different actor | any | **New origin** — not drift |

**Key constraint**: No controller modifications required. All logic runs in admission.

## Documentation

| Document | Topics |
|----------|--------|
| [DESIGN_DRIFT_DETECTION.md](DESIGN_DRIFT_DETECTION.md) | Drift detection mechanism, controller identification, annotation protection, lifecycle phases |
| [DESIGN_APPROVALS.md](DESIGN_APPROVALS.md) | Approval/rejection annotations, modes, enforcement, freeze/snooze, ApprovalPolicy CRD |
| [DESIGN_TRACING.md](DESIGN_TRACING.md) | Request tracing, origin vs controller hop, trace labels |
| [DESIGN_CALLBACKS.md](DESIGN_CALLBACKS.md) | Drift notification webhooks, DriftReport API, Slack escalation |
| [DESIGN_DEPLOYMENT.md](DESIGN_DEPLOYMENT.md) | Library vs webhook deployment, resource targeting, Helm configuration |
| [ADR.md](ADR.md) | Architecture decisions, rationale, trade-offs, alternatives |
| [ROADMAP.md](ROADMAP.md) | Implementation phases and status |

## Quick Reference

### Annotations

| Annotation | Purpose |
|------------|---------|
| `kausality.io/trace` | Causal chain of mutations (JSON array) |
| `kausality.io/updaters` | Hashes of users who update spec |
| `kausality.io/controllers` | Hashes of users who update status |
| `kausality.io/approvals` | Pre-approved child mutations |
| `kausality.io/rejections` | Explicitly blocked mutations |
| `kausality.io/freeze` | Emergency lockdown (blocks ALL changes) |
| `kausality.io/snooze` | Suppress drift callbacks until expiry |
| `kausality.io/mode` | `log` or `enforce` |

### Admission Flow Summary

```
1. Receive mutation (CREATE/UPDATE/DELETE)
2. Find parent via controller ownerReference
3. Short-circuit checks:
   - Parent deleting → ALLOW
   - Parent initializing → ALLOW
   - Parent frozen → DENY
4. If parent reconciling (gen != obsGen) → ALLOW (expected)
5. Else (drift):
   - Check rejections → DENY if matched
   - Check approvals → ALLOW if matched
   - Check ApprovalPolicy → ALLOW if matched
   - Else → DENY + escalate
```
