package approval

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CheckResult contains the result of an approval check.
type CheckResult struct {
	// Approved indicates the mutation is approved.
	Approved bool
	// Rejected indicates the mutation is explicitly rejected.
	Rejected bool
	// Reason explains the decision.
	Reason string
	// MatchedApproval is the approval that matched (if any).
	// Used for consuming mode=once approvals.
	MatchedApproval *Approval
	// MatchedRejection is the rejection that matched (if any).
	MatchedRejection *Rejection
}

// Checker checks if a child mutation is approved or rejected.
type Checker struct{}

// NewChecker creates a new Checker.
func NewChecker() *Checker {
	return &Checker{}
}

// Check checks if a mutation to the given child is approved or rejected.
// It reads approvals/rejections from the parent's annotations.
//
// Priority:
// 1. Rejection (if matched) - returns Rejected=true
// 2. Approval (if matched and valid) - returns Approved=true
// 3. Neither - returns Approved=false, Rejected=false
func (c *Checker) Check(parent client.Object, child ChildRef, parentGeneration int64) CheckResult {
	annotations := parent.GetAnnotations()
	if annotations == nil {
		return CheckResult{
			Reason: "no approvals or rejections on parent",
		}
	}

	// Check rejections first (rejection wins)
	if result := c.checkRejections(annotations, child, parentGeneration); result.Rejected {
		return result
	}

	// Check approvals
	return c.checkApprovals(annotations, child, parentGeneration)
}

// checkRejections checks if the child is rejected.
func (c *Checker) checkRejections(annotations map[string]string, child ChildRef, parentGeneration int64) CheckResult {
	rejectionsStr := annotations[RejectionsAnnotation]
	if rejectionsStr == "" {
		return CheckResult{}
	}

	rejections, err := ParseRejections(rejectionsStr)
	if err != nil {
		return CheckResult{
			Reason: "failed to parse rejections: " + err.Error(),
		}
	}

	for i := range rejections {
		r := &rejections[i]
		if r.Matches(child) && r.IsActive(parentGeneration) {
			return CheckResult{
				Rejected:         true,
				Reason:           r.Reason,
				MatchedRejection: r,
			}
		}
	}

	return CheckResult{}
}

// checkApprovals checks if the child is approved.
func (c *Checker) checkApprovals(annotations map[string]string, child ChildRef, parentGeneration int64) CheckResult {
	approvalsStr := annotations[ApprovalsAnnotation]
	if approvalsStr == "" {
		return CheckResult{
			Reason: "no approval found for child",
		}
	}

	approvals, err := ParseApprovals(approvalsStr)
	if err != nil {
		return CheckResult{
			Reason: "failed to parse approvals: " + err.Error(),
		}
	}

	for i := range approvals {
		a := &approvals[i]
		if a.Matches(child) {
			if a.IsValid(parentGeneration) {
				return CheckResult{
					Approved:        true,
					Reason:          "approved via " + a.Mode + " approval",
					MatchedApproval: a,
				}
			}
			// Matched but not valid (stale generation)
			return CheckResult{
				Reason: "approval found but invalid (stale generation)",
			}
		}
	}

	return CheckResult{
		Reason: "no approval found for child",
	}
}

// CheckFromAnnotations is a convenience function that checks approvals
// directly from annotation strings.
func CheckFromAnnotations(approvalsStr, rejectionsStr string, child ChildRef, parentGeneration int64) CheckResult {
	// Check rejections first
	if rejectionsStr != "" {
		rejections, err := ParseRejections(rejectionsStr)
		if err == nil {
			for i := range rejections {
				r := &rejections[i]
				if r.Matches(child) && r.IsActive(parentGeneration) {
					return CheckResult{
						Rejected:         true,
						Reason:           r.Reason,
						MatchedRejection: r,
					}
				}
			}
		}
	}

	// Check approvals
	if approvalsStr != "" {
		approvals, err := ParseApprovals(approvalsStr)
		if err == nil {
			for i := range approvals {
				a := &approvals[i]
				if a.Matches(child) && a.IsValid(parentGeneration) {
					return CheckResult{
						Approved:        true,
						Reason:          "approved via " + a.Mode + " approval",
						MatchedApproval: a,
					}
				}
			}
		}
	}

	return CheckResult{
		Reason: "no approval found for child",
	}
}
