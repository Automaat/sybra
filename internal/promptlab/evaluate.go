package promptlab

// EvaluateProposal runs a proposal's candidate through ev and sets the
// tri-state offline result plus the human-approval gate. NoVerdict is
// fail-closed — treated exactly like Failed, it forces human approval — see
// OfflineEvaluator's doc comment for why a resolved-text runner can't
// produce a real Passed verdict at this stage.
func EvaluateProposal(p Proposal, ev OfflineEvaluator) Proposal {
	if ev == nil {
		ev = stubEvaluator{}
	}
	p.Offline = ev.Evaluate(p.Candidate)
	p.RequiresHumanApproval = p.Offline.Verdict != VerdictPassed
	return p
}
