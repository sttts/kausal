# Request Tracing

## Overview

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

## Origin vs Controller Hop

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

## Trace Lifecycle

- **Created** when a mutation has no parent trace to extend
- **Extended** when a controller propagates changes to children
- **Replaced** when parent generation changes (new causal chain starts)

## Trace Labels

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
