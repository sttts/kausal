# Kausal

A system for tracing and gating spec changes through a hierarchy of KRM objects and downstream systems (e.g., Terraform). Controllers cannot mutate downstream unless explicitly allowed.

## Goals

**This is about safety, not security.**

We want a best-effort system that stays out of the way of the user, but enables:
- Traceability of destructive actions
- Avoidance of accidental damage where possible

The system assumes good intent. It protects against accidents, not malicious actors.

## Core Concepts

### Allowance

A permission for a controller to perform a specific mutation on a child object.

- Stored in parent object's annotations, protected by admission
- Additive: allowances accumulate without conflict
- Carries causation trace back to origin (the initiator)

### AllowancePolicy

A CRD defining rules that map parent field changes to permitted child mutations. Evaluated by the admission webhook.

## Allowance Storage

Allowances are stored in annotations to remain controller-agnostic:

```yaml
kind: Deployment
metadata:
  annotations:
    kausal.io/allowances: |
      - kind: ReplicaSet           # child kind the controller may mutate
        mutation: spec.replicas    # field the controller may change on child
        generation: 7              # generation of this object that caused it
        initiator: hans@example.com  # human/CI that started the chain
        trace:                     # causation path to this point
        - kind: Deployment
          name: foo
          generation: 7
          field: spec.replicas     # field change that triggered this
```

Propagated to child (ReplicaSet allows Pod mutations):

```yaml
kind: ReplicaSet
metadata:
  annotations:
    kausal.io/allowances: |
      - kind: Pod                  # child kind the controller may mutate
        mutation: delete           # operation permitted on child
        generation: 14             # generation of this object that caused it
        initiator: hans@example.com
        trace:
        - kind: Deployment
          name: foo
          generation: 7
          field: spec.replicas
        - kind: ReplicaSet         # this object, appended by admission
          name: foo-abc
          generation: 14
          field: spec.replicas
```

### Trace

The trace is a linear causation path from initiator to current object.

- **Linearity**: When multiple allowances could justify a mutation, one is chosen deterministically (e.g., alphabetic by `{kind}/{name}/{field}`)
- **Initiator**: The human or CI system that started the chain (only captured once, at the top level)
- **Hops**: Each intermediate object that propagated the allowance
- **Field**: Full JSON path including concrete indices (e.g., `spec.template.spec.containers[0].image`)
- **Attestations**: Optional captured values from the object (e.g., Jira ticket)

Namespace is omitted from trace hops — it's always the same as the object carrying the allowance (or cluster-scoped).

#### Attestations (Optional Extension)

Traces can capture external references as proof:

```yaml
kind: Deployment
metadata:
  annotations:
    kausal.io/allowances: |
      - kind: ReplicaSet
        mutation: spec.replicas
        generation: 7
        initiator: hans@example.com
        trace:
        - kind: Deployment
          name: foo
          generation: 7
          field: spec.replicas
          attestations:                              # captured values
            "metadata.annotations[jira]": "INFRA-23232"
            "metadata.annotations[approved-by]": "alice@example.com"
```

The AllowancePolicy specifies which fields to capture via `capture`:

```yaml
spec:
  match:
    kind: Deployment
    fields: ["spec.replicas"]
    cel:
    - "has(object.metadata.annotations['jira'])"
    capture:                                         # fields to store in trace
    - "metadata.annotations[jira]"
    - "metadata.annotations[approved-by]"
```

- `cel` validates the field exists or matches a pattern
- `capture` stores the actual value in the trace

The trace becomes self-documenting — auditable without looking up external systems.

### Consumption

Allowances are consumed based on `status.observedGeneration`:

- If `status.observedGeneration >= allowance.generation` → allowance is consumed
- Consumed allowances can be pruned by the next admission

This leverages existing controller behavior — no changes required to controllers.

## AllowancePolicy

```yaml
kind: AllowancePolicy
apiVersion: kausal.io/v1alpha1
metadata:
  name: deployment-to-replicaset
spec:
  match:
    kind: Deployment
    fields:
    - "spec.replicas"
    - "spec.template.spec.containers[*].image"    # wildcards supported
    cel:
    - "has(object.spec.jiraTicket)"
    - "object.metadata.labels['env'] != 'prod' || has(object.spec.jiraTicket)"

  subjects:
  - kind: Group
    name: platform-team
    mayInitiate: true
  - kind: ServiceAccount
    name: deployment-controller
    namespace: kube-system

  allow:
  - kind: ReplicaSet
    relation: Child
    mutations: ["spec.replicas", "spec.template"]
```

### match

Defines when this policy applies.

| Field | Description |
|-------|-------------|
| `kind` | Parent object kind |
| `fields` | Spec fields that trigger this rule (supports `[*]` wildcards) |
| `cel` | CEL expressions for additional conditions (optional) |
| `capture` | Fields to capture in the trace as attestations (optional) |

CEL expressions have access to `object` and `oldObject`. Use cases:
- Require external references: `has(object.spec.jiraTicket)`
- Pattern validation: `object.spec.jiraTicket.matches('^INFRA-[0-9]+$')`
- Conditional requirements: `object.metadata.labels['env'] != 'prod' || has(object.spec.jiraTicket)`
- Change direction: `object.spec.replicas > oldObject.spec.replicas`

Wildcards in policy fields (e.g., `[*]`) match any index; traces record the actual index.

### subjects

Who may trigger this rule. Follows RBAC subject conventions.

| Field | Description |
|-------|-------------|
| `kind` | `User`, `Group`, or `ServiceAccount` |
| `name` | Subject name |
| `namespace` | For ServiceAccount only |
| `mayInitiate` | If `true`, can start a new allowance chain (not just propagate) |

Subjects without `mayInitiate` can only propagate allowances that already exist on a parent object.

### allow

List of permitted downstream mutations when the policy matches.

| Field | Description |
|-------|-------------|
| `kind` | Target child object kind |
| `relation` | `Child` (via ownerRef) |
| `mutations` | Permitted operations: field paths or `delete`, `create` |

## Admission Flow

```
Human changes Deployment.spec.replicas
         |
         v
+---------------------------------------------+
| Admission Webhook (on Deployment)           |
|                                             |
| 1. Evaluate AllowancePolicies               |
| 2. Check subjects + mayInitiate             |
| 3. Check CEL conditions                     |
| 4. Inject allowances into annotations       |
+---------------------------------------------+
         |
         v
Controller mutates ReplicaSet
         |
         v
+---------------------------------------------+
| Admission Webhook (on ReplicaSet)           |
|                                             |
| 1. Find parent via ownerRef                 |
| 2. Check parent has matching allowance      |
| 3. Check subject (SA) is permitted          |
| 4. Inject downstream allowances             |
| 5. Prune consumed allowances on parent      |
+---------------------------------------------+
         |
         v
        ...
```

## Primitives

- Allowances stored on objects are protected by admission
- A mutation can only add allowances if:
  - Parent object has a matching allowance AND policy permits propagation, OR
  - Policy matches with a `mayInitiate` subject
- User identity comes from Kubernetes authentication (not self-declared)
- Default-deny: without a matching allowance, controllers cannot mutate downstream
