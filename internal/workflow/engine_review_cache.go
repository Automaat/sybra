package workflow

import (
	"encoding/json"
	"fmt"

	"github.com/Automaat/sybra/internal/evidence"
)

// Hash only verification inputs, not mutable results such as review sidecars
// or the acceptance ledger. Never persist raw work-derived contract content.
func (e *Engine) verificationContractDigest(t TaskInfo) string {
	t = e.withManualTestConfig(t)
	data, err := json.Marshal([]any{t.ProjectID, t.Title, t.Body, t.Plan, t.PlanContract,
		t.ManualTest.Kind, t.ManualTest.Command, t.ManualTest.HealthURL, t.ManualTest.ProbeCommands})
	if err != nil {
		return ""
	}
	return evidence.Digest(string(data))
}

func verificationInputKey(stepID, input string) string {
	return "evidence." + stepID + "." + input
}

func (e *Engine) bindVerificationInput(taskID string, step *Step, wf *Execution, t TaskInfo) error {
	if step.Config.Role != reviewAgentRole && step.Config.Role != testRunnerRole {
		return nil
	}
	wf.SetVar(verificationInputKey(step.ID, "head"), e.currentHeadSHA(e.ctx, taskID))
	wf.SetVar(verificationInputKey(step.ID, "contract"), e.verificationContractDigest(t))
	policy, err := e.ciPolicy(e.ctx, taskID)
	if err != nil {
		return err
	}
	if policy == nil || !policy.Enabled {
		return nil
	}
	if wf.Variables[verificationInputKey(step.ID, "head")] == "" {
		return fmt.Errorf("verification input HEAD unavailable before dispatch")
	}
	// Persist before spending a provider call. A fast completion or a failed
	// route-publication write must still find the exact input it verified.
	return e.tasks.SetWorkflow(taskID, wf)
}

func (e *Engine) reusableReview(t TaskInfo) bool {
	if !t.Reviewed || t.CodeReviewVerdict != "CLEAN" || e.evidenceRecorder == nil {
		return false
	}
	ce, err := e.evidenceRecorder.Evidence(t.ID)
	if err != nil {
		return false
	}
	entry, ok := ce.ByCriterion(evidenceCriterionReview)
	return ok && entry.Passed() && entry.FinalRev != "" && entry.FinalRev == e.currentHeadSHA(e.ctx, t.ID) &&
		entry.ContractDigest != "" && entry.ContractDigest == e.verificationContractDigest(t)
}
