# Drift Detection

## Detection Mechanism

When a mutation is intercepted, admission:

1. **Identifies the actor** using user hash tracking (comparing request user with recorded controller hashes)
2. **Checks the parent's state** (`generation` vs `observedGeneration`)

```
parent := resolve via controller ownerReference (controller: true)
isController := IsControllerByHash(parent.controllers, child.updaters, request.user)

if NOT isController:
    # Different actor → new causal origin (not drift)
    # Start new trace, currently allowed (ApprovalPolicy CRD planned)
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
| Different actor | any | **New origin** — not drift, allowed (ApprovalPolicy planned) |

**Drift** specifically means: the controller is making changes when the parent spec hasn't changed (gen == obsGen). This indicates unexpected reconciliation triggered by external factors (drift in cloud state, software updates, etc.).

**Different actors** (kubectl, HPA, GitOps tools) are not considered drift — they're simply different causal chains that create new trace origins. Currently these are allowed; a planned ApprovalPolicy CRD will enable restricting certain actors.

**Spec changes only**: Kausality only processes spec mutations for drift detection and tracing. Status subresource updates are intercepted solely to record controller identity (adding user hash to the `controllers` annotation). Metadata-only changes don't trigger drift detection or tracing.

## Controller Identification

A key challenge is identifying whether a mutation comes from the controller (expected) or another actor (potential drift). We use **user hash tracking** for this.

**The controller is identified by correlating users who update parent status with users who update child spec.**

**Annotations:**
- Parent: `kausality.io/controllers` — 5-char base36 hashes of users who update status (max 5)
- Child: `kausality.io/updaters` — 5-char base36 hashes of users who update spec (max 5)

**Recording:**
- Child CREATE/UPDATE (spec change only): user hash added to child's `updaters` annotation (sync, via patch)
- Parent status UPDATE: user hash added to parent's `controllers` annotation (sync + async backup)

**Important:** Metadata-only changes (labels, annotations) do NOT record updaters. Only actual spec changes add the user to the updaters list. This ensures that users who only modify metadata are not incorrectly identified as controllers.

**Detection algorithm:**

```
if child has 1 updater:
    controller = that single updater
else if parent has controllers annotation:
    controller = intersection(child.updaters, parent.controllers)
else:
    → can't determine, be lenient (skip drift detection)

if current_user_hash in controller set → controller request → check drift
else → not controller → not drift (new causal origin)
```

**Why user hash tracking instead of fieldManager?**
- Works reliably across all request types
- User identity is always available in admission requests
- Doesn't depend on clients setting fieldManager correctly
- 5-char hashes keep annotations compact

**Late installation:** On first run, parent won't have `kausality.io/controllers`. The system is lenient when it can't determine controller identity, allowing the annotation to build up over time.

**Non-owning controllers (HPA, VPA):** These don't set controller ownerReferences. They appear as different actors and create new trace origins. This is NOT drift — it's simply a different causal chain. Currently these are allowed; a planned ApprovalPolicy CRD will enable restricting or explicitly allowing certain actors.

**Webhook configuration:** Must intercept status subresource updates to record controller identity on parents.

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

### Phase Annotation

The `kausality.io/phase` annotation tracks the lifecycle phase of parent resources:

```yaml
kausality.io/phase: "initializing" | "initialized"
```

**Phase values:**
- `initializing` — Resource not yet ready (no Ready/Synced=True, no observedGeneration match)
- `initialized` — Resource reached steady state (persisted "high water mark")
- `deleting` — Not stored; derived from `deletionTimestamp` in metadata

**Key behavior:** Once `phase=initialized` is written, it is NOT downgraded to `initializing` even if conditions flip. The annotation persists the "high water mark" to handle flapping conditions (e.g., Crossplane Ready).

```
Transition rules:
- initializing → initialized: Store "initialized"
- initialized → initializing: NO CHANGE (keep "initialized")
- any → deleting: Don't store (derived from deletionTimestamp)
```

### Phase Recording

Phase is recorded asynchronously via three triggers:

| Trigger | When | Method | Rationale |
|---------|------|--------|-----------|
| Parent status UPDATE | Status subresource update | Async | Conditions may have changed |
| Child CREATE/UPDATE | Processing child request | Async (lazy) | Already computed phase for drift detection |

**Lazy fetch for child requests:** When processing child requests, the parent's phase is already computed for drift detection. The system only fetches the parent to record phase if:
1. The computed phase is `initialized`, AND
2. The parent doesn't already have `phase=initialized`

This avoids API calls on steady-state child updates while ensuring phase is eventually recorded.

### Initialization Detection

Detection priority for determining if a resource is initialized (default, configurable per GVK):

1. `kausality.io/phase: "initialized"` annotation exists
2. `Initialized=True` condition exists
3. `Ready=True` condition exists
4. `status.observedGeneration` matches `generation` AND `Ready=True`

The fourth check specifically requires `Ready=True` (not just `Synced=True`) because:
- Crossplane sets `Synced=True` when it sends requests to create children
- But `Ready=False` until children are actually created and ready
- During child creation, the composition controller needs to update children
- Those updates should be allowed until `Ready=True` (full hierarchy is ready)

During initialization, all child changes are allowed (including CREATE).

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
     c. Else:
        - If parent has snooze annotation and not expired → suppress callbacks
        - Send drift callback (if not snoozed)
        - In enforce mode: DENY
        - In log mode: ALLOW with warning
```

## Response Codes

| Outcome | Response |
|---------|----------|
| Parent deleting | `allowed: true` |
| Parent initializing | `allowed: true` |
| Parent frozen | `allowed: false`, status 403 Forbidden, message includes user/reason/timestamp from freeze annotation |
| Expected change (gen != obsGen) | `allowed: true` |
| Drift with valid approval | `allowed: true` |
| Drift rejected (explicit rejection) | `allowed: false`, status 403 Forbidden, reason from rejection |
| Drift snoozed | Callbacks suppressed until expiry, mutations still follow normal drift rules |
| Drift without approval (enforce mode) | `allowed: false`, status 403 Forbidden, sends drift callback |
| Drift without approval (log mode) | `allowed: true` with warning, sends drift callback |
| No controller ownerReference | `allowed: true` (not a controller-managed child) |
| Error resolving parent | `allowed: false`, status 500 Internal Server Error |
