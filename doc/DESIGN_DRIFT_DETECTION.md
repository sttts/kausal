# Drift Detection

## Detection Mechanism

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

## Controller Identification

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

## Annotation Protection from Controller Sync

Kubernetes controllers (e.g., deployment-controller) copy annotations from parent to child on both CREATE and UPDATE. This overwrites kausality's computed annotations with stale values from the parent.

**Problem sequence:**
1. Deployment CREATE → webhook adds 1-hop trace
2. RS CREATE → controller copies Deployment's 1-hop trace, webhook patches to 2-hop
3. RS stored with correct 2-hop trace
4. Controller sees RS annotations differ from Deployment
5. Controller UPDATEs RS with 1-hop annotations → **overwrites our values**

**Solution:** Three compute functions handle annotations for different update types.

**Annotation categories:**
- **System annotations** (`trace`, `updaters`, `controllers`): Special handling based on context
- **User annotations** (`approvals`, `rejections`, `freeze`, `snooze`, `trace-*`): Always preserved from OldObject on controller updates

**Compute functions:**

| Function | Use Case | System annotations | User annotations |
|----------|----------|-------------------|------------------|
| `computeAnnotationsForController` | Controller spec/metadata updates | Recompute (spec change) or preserve (no spec change) | Preserve from old |
| `computeAnnotationsForUser` | User spec updates | New origin (spec change) or preserve (no spec change) | User can modify |
| `computeAnnotationsForStatusUpdate` | Status subresource updates | Preserve + add user to controllers | Preserve from old |

**UPDATE handling:**

| Scenario | System annotations | User annotations |
|----------|-------------------|------------------|
| No spec change | Preserve from OldObject | Preserve from OldObject |
| Controller + spec change | Recompute trace/updaters, preserve controllers | Preserve from OldObject |
| User + spec change | New origin (trace/updaters) | Normal (user can modify) |

**Child as parent:** A ReplicaSet can be both a child (of Deployment) and a parent (of Pods). The `controllers` annotation (who updates status) is always preserved on child spec updates since it's not recomputed for the child role.

**Status updates:** Recording controller identity is synchronous via patch (adds user hash to `controllers` annotation), with async backup update via `RecordControllerAsync`.

**Key insight:** No spec change = no legitimate reason to change annotations. Always preserve all kausality annotations from OldObject unconditionally.

**CREATE handling:** Wipe all `kausality.io/*` annotations copied from parent, then compute fresh values.

**For Terraform (L0 controllers)**:
- Check if plan is non-empty when generation == observedGeneration
- Use drift notification webhooks for plan review workflows

## Lifecycle Phases

### Initialization

During initialization, all child changes are allowed (including CREATE). Detection priority (default, configurable per GVK):

1. `Initialized=True` condition exists
2. `Ready=True` condition exists (with persistence — see below)
3. `status.observedGeneration` exists

Once initialized, admission stores:
```yaml
kausality.io/initialized: "true"
```

This persists the initialized state for resources with flapping conditions (e.g., Crossplane Ready).

### Deletion

When parent has `metadata.deletionTimestamp`:
- Allow ALL child mutations (cleanup phase)
- No drift checks, no approvals needed

## Operations by Type

| Operation | Drift Rules |
|-----------|-------------|
| CREATE | Allowed during initialization. Blocked during drift (requires approval). |
| UPDATE | Blocked during drift unless approved. |
| DELETE | Blocked during drift unless approved (same as UPDATE). |

## Admission Flow

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
        - If parent has snooze annotation and not expired → DENY (no escalation)
        - Else → DENY and escalate to Slack
```

## Response Codes

| Outcome | Response |
|---------|----------|
| Parent deleting | `allowed: true` |
| Parent initializing | `allowed: true` |
| Parent frozen | `allowed: false`, status 403 Forbidden, message includes user/reason/timestamp from freeze annotation |
| Expected change (gen != obsGen) | `allowed: true` |
| Drift with valid approval | `allowed: true` |
| Drift with ApprovalPolicy match | `allowed: true` |
| Drift rejected (explicit rejection) | `allowed: false`, status 403 Forbidden, reason from rejection |
| Drift snoozed | Callbacks suppressed until expiry, mutations still follow normal drift rules |
| Drift without approval (blocked) | `allowed: false`, status 403 Forbidden, escalate to Slack |
| Parent not found | `allowed: false`, status 422 Unprocessable |
