# Implementation Roadmap

## Phase 1: Admission-Based Change Detection (Logging Only) ✓
- Implement admission webhook/plugin
- Check parent generation vs observedGeneration
- Log when drift would be blocked
- No blocking, observability only

## Phase 2: Request-Trace Annotation ✓
- Initialize request-trace on parent mutations
- Propagate/replace trace through controller hierarchy

## Phase 3: Per-Object Approval Annotation ✓
- [x] Implement approval annotation parsing with modes (once, generation, always)
- [x] Implement rejection annotation parsing
- [x] Rejection checking (rejection wins over approval)
- [x] Approval pruning logic (prune stale generations, consume once)
- [x] Integration with admission handler (logging mode)
- [x] Enforce mode (per-G/GR configuration via Helm)
- [x] Approval pruning via admission mutation (update annotations)

## Phase 4: Drift Notification Webhook System ✓
- [x] Implement DriftReport webhook callback (kausality.io/v1alpha1)
- [x] Content-based deduplication (ID hash)
- [x] Send phase=Resolved on drift resolution
- [x] Action helpers for webhook implementations
- [x] Backend implementations (kausality-backend-log, kausality-backend-tui)
- [x] Helm chart integration with backend deployment

## Phase 5: ApprovalPolicy CRD
- Define and implement ApprovalPolicy CRD
- Pattern-based exceptions (reduce per-object approval burden)
- Namespace-scoped policies, ClusterApprovalPolicy for cluster-wide rules

## Phase 6: Slack Integration
- Integrate Slack escalation workflow
- Implement approval/rejection via Slack bot
- "Add Exception" dialog to create ApprovalPolicy rules
