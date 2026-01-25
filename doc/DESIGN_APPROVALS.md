# Approvals and Enforcement

## Approval and Rejection Annotations

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

## Approval Modes

| Mode | Behavior | Use Case |
|------|----------|----------|
| `once` | Removed after first allowed mutation | One-time drift fix, strict control |
| `generation` | Valid while `parent.generation == approval.generation` | Approve for current state, invalidate on spec change |
| `always` | Permanent, never automatically pruned | Known-safe pattern, permanent exception |

## Rejection Priority

**Rejections are checked before approvals.** If a child has both an approval and a rejection, the rejection wins. This ensures explicit blocks cannot be accidentally bypassed.

## Approval Validity

An approval is valid when:
1. No matching rejection exists for this child
2. `approval.apiVersion/kind/name` matches the child being mutated
3. Mode-specific:
   - `once`: not yet consumed AND `approval.generation == parent.generation`
   - `generation`: `approval.generation == parent.generation`
   - `always`: always valid

## Pruning Rules

| Trigger | Effect |
|---------|--------|
| Parent generation changes | `once` and `generation` approvals with `generation < parent.generation` are pruned |
| Approval used (`mode: once`) | That specific approval is removed |
| `mode: always` | Never pruned automatically (explicit removal required) |

## Enforcement Mode

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

## Freeze and Snooze

Additional parent annotations for operational control:

```yaml
metadata:
  annotations:
    # Freeze: block ALL changes (emergency lockdown)
    kausality.io/freeze: '{"user":"admin@example.com","message":"investigating incident #123","at":"2026-01-25T10:00:00Z"}'
    # Snooze: suppress drift callbacks until expiry
    kausality.io/snooze: '{"expiry":"2026-01-25T12:00:00Z","user":"ops@example.com","message":"deploying hotfix"}'
```

| Annotation | Effect |
|------------|--------|
| `kausality.io/freeze` | Block ALL child mutations, even expected changes. Emergency lockdown. Structured JSON with `user`, `message`, `at` fields for audit trail. |
| `kausality.io/snooze` | Suppress drift callbacks until `expiry` timestamp. Structured JSON with `user`, `message` fields. Does not block mutations, only notifications. |

Both annotations support legacy formats for backwards compatibility (`"true"` for freeze, plain RFC3339 timestamp for snooze).

## ApprovalPolicy CRD

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
