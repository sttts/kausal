package approval

// Pruner removes stale or consumed approvals.
type Pruner struct{}

// NewPruner creates a new Pruner.
func NewPruner() *Pruner {
	return &Pruner{}
}

// ConsumeOnce removes a mode=once approval from the list after it's used.
// Returns the updated list and true if an approval was consumed.
func (p *Pruner) ConsumeOnce(approvals []Approval, consumed *Approval) ([]Approval, bool) {
	if consumed == nil {
		return approvals, false
	}

	mode := consumed.Mode
	if mode == "" {
		mode = ModeOnce
	}
	if mode != ModeOnce {
		return approvals, false
	}

	// Find and remove the consumed approval
	result := make([]Approval, 0, len(approvals))
	found := false
	for _, a := range approvals {
		if !found && a.Matches(ChildRef{
			APIVersion: consumed.APIVersion,
			Kind:       consumed.Kind,
			Name:       consumed.Name,
		}) && a.Generation == consumed.Generation && a.Mode == consumed.Mode {
			found = true
			continue // Skip this one (consume it)
		}
		result = append(result, a)
	}

	return result, found
}

// PruneStale removes approvals that are stale due to parent generation change.
// Removes mode=once and mode=generation approvals where approval.generation < parentGeneration.
// mode=always approvals are never pruned.
func (p *Pruner) PruneStale(approvals []Approval, parentGeneration int64) []Approval {
	result := make([]Approval, 0, len(approvals))

	for _, a := range approvals {
		mode := a.Mode
		if mode == "" {
			mode = ModeOnce
		}

		switch mode {
		case ModeAlways:
			// Never prune
			result = append(result, a)
		case ModeOnce, ModeGeneration:
			// Keep only if generation matches current parent generation
			if a.Generation >= parentGeneration {
				result = append(result, a)
			}
			// Otherwise it's stale, don't include
		default:
			// Unknown mode - keep it to be safe
			result = append(result, a)
		}
	}

	return result
}

// PruneResult contains the result of pruning operations.
type PruneResult struct {
	// Approvals is the updated list after pruning.
	Approvals []Approval
	// Changed indicates if any approvals were removed.
	Changed bool
	// RemovedCount is the number of approvals removed.
	RemovedCount int
}

// Prune performs both consume and stale pruning in one operation.
// Use this when processing a successful mutation with mode=once approval.
func (p *Pruner) Prune(approvals []Approval, consumed *Approval, parentGeneration int64) PruneResult {
	originalLen := len(approvals)

	// First consume the used approval
	result, _ := p.ConsumeOnce(approvals, consumed)

	// Then prune stale approvals
	result = p.PruneStale(result, parentGeneration)

	return PruneResult{
		Approvals:    result,
		Changed:      len(result) != originalLen,
		RemovedCount: originalLen - len(result),
	}
}
