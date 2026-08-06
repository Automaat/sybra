package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/taskstatus"
)

// admissionPreflightReasonPrefix marks a human-required status_reason as
// having been written by admission_preflight, mirroring
// TamperFlaggedReasonPrefix/verifyBlessedTag's pattern for other mechanical
// gates — lets health checks and dashboards group admission blocks distinctly
// from other human-required causes.
const admissionPreflightReasonPrefix = "admission preflight blocked:"

// execAdmissionPreflight runs deterministic pre-dispatch admission checks
// before any code-author agent is dispatched: it is wired as the first step
// of simple-task-implement, so it covers both the planned path (a plan
// contract already validated once by validate_plan_contract during planning)
// and the noplan/handoff paths (a task that reaches in-progress without ever
// running that step — e.g. a simple task, or `sybra-cli handoff --stage
// implement`).
//
// Checks, in order:
//  1. Plan contract validity (schema + admission-facts fields under
//     PlanContractSchemaV2) via the same ValidatePlanContractForTask the
//     planning-time gate uses — a no-op for an absent contract (the
//     markdown-only migration fallback, matching execValidatePlanContract).
//  2. Oversize scope, when the app config sets a non-zero limit: the
//     contract's acceptance_criteria/files counts against
//     admission.MaxAcceptanceCriteria/MaxChangeSurfaceFiles. Zero (the
//     default) disables both — auto-splitting an oversized task into a gated
//     DAG is deferred (#2466 Fix point 5); this only flags it for a human.
//  3. Push credential readiness via the same preflighter push_branch/
//     create_pr already use (e.factory.pushPreflight, falling back to
//     project.PreflightPushCredentials) — but ONLY when a worktree already
//     exists for the task. A fresh task dispatched straight from planning (or
//     a noplan task's very first run) has no worktree yet at this point —
//     PrepareForTask creates it lazily inside the `implement` run_agent step
//     itself — so a missing worktree here is not yet a failure; skipping it
//     avoids flipping every ordinary task to human-required on its first
//     pass (see the Stop Condition on mass-flipping in-flight tasks).
//
// A contract/scope failure sets a terminal blocker.State (Exhausted: true — no
// automatic re-attempts, matching KindTriageRetryExhausted's precedent) and
// flips the task to human-required with blocker.KindOperatorDecision (a human
// must fix the spec, not retry the same dispatch).
//
// A push-credential failure is NOT unconditionally terminal: it is classified
// the same way push_branch/create_pr classify the identical preflight error
// (classifyAdmissionCredentialError), so a rate-limit or transient GitHub blip
// hit at this step self-heals via a bounded retry instead of permanently
// stranding a re-dispatched task at human-required. Only a non-transient
// credential failure escalates, as blocker.KindCredentialRequired. Both
// blocker kinds are in blocker.AllowsHumanRequired's allow-list.
//
// Disabled via admission.Enabled=false (SetAdmissionConfig) is a pure no-op —
// the step always admits.
func (e *Engine) execAdmissionPreflight(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	if !e.admission.Enabled {
		return e.admitTask(step, t, "disabled")
	}

	raw := strings.TrimSpace(t.PlanContract)
	if raw != "" {
		if problems := ValidatePlanContractForTask(raw, taskID, t.Body); len(problems) > 0 {
			reason := "plan contract invalid: " + strings.Join(problems, "; ")
			return e.blockAdmission(taskID, step, t, blocker.KindOperatorDecision, reason)
		}
		if reason := e.checkAdmissionOversize(raw); reason != "" {
			return e.blockAdmission(taskID, step, t, blocker.KindOperatorDecision, reason)
		}
	}

	if credErr := e.checkAdmissionCredentials(taskID); credErr != nil {
		return e.classifyAdmissionCredentialError(taskID, step, wfExec, t, credErr)
	}

	return e.admitTask(step, t, "admitted")
}

// classifyAdmissionCredentialError mirrors classifyPRGitError's transient-vs-
// permanent split for the push-credential preflight run at admission time: a
// rate-limit or transient GitHub blip parks the step for a bounded retry (it
// self-heals on a later pass, exactly as push_branch/create_pr would for the
// identical error moments later in the same workflow), while a genuine auth
// failure or otherwise unclassifiable credential problem escalates to a
// terminal blocker.KindCredentialRequired — the same distinction
// looksLikeAuthFailure draws (a broken token does not self-heal, a network
// blip does).
func (e *Engine) classifyAdmissionCredentialError(taskID string, step *Step, wfExec *Execution, t TaskInfo, err error) (StepOutput, error) {
	msg := err.Error()
	switch {
	case looksLikeGitHubRateLimit(msg):
		return e.parkStepForRetry(taskID, wfExec, t, step.ID, prCreateRetryStatusReason, "workflow.admission-preflight.rate-limit")
	case looksLikeTransientGitHub(msg):
		return e.parkStepForRetry(taskID, wfExec, t, step.ID, prCreateTransientStatusReason, "workflow.admission-preflight.transient")
	default:
		return e.blockAdmission(taskID, step, t, blocker.KindCredentialRequired,
			"push credential preflight failed: "+trimDiffLine(msg))
	}
}

// checkAdmissionOversize returns a non-empty reason when the plan contract's
// acceptance-criteria or file count exceeds a configured (non-zero) limit.
// raw has already round-tripped through ValidatePlanContractForTask
// successfully, so the re-parse here cannot fail.
func (e *Engine) checkAdmissionOversize(raw string) string {
	if e.admission.MaxAcceptanceCriteria <= 0 && e.admission.MaxChangeSurfaceFiles <= 0 {
		return ""
	}
	contract, err := parsePlanContract(raw)
	if err != nil {
		return ""
	}
	if limit := e.admission.MaxAcceptanceCriteria; limit > 0 && len(contract.AcceptanceCriteria) > limit {
		return fmt.Sprintf(
			"acceptance_criteria count %d exceeds configured limit %d — split into smaller tasks (auto-split is deferred, see #2466 Fix point 5) or raise admission.max_acceptance_criteria",
			len(contract.AcceptanceCriteria), limit)
	}
	if limit := e.admission.MaxChangeSurfaceFiles; limit > 0 && len(contract.Files) > limit {
		return fmt.Sprintf(
			"files count %d exceeds configured change-surface limit %d — split into smaller tasks (auto-split is deferred, see #2466 Fix point 5) or raise admission.max_change_surface_files",
			len(contract.Files), limit)
	}
	return ""
}

// checkAdmissionCredentials returns a non-nil error when a worktree already
// exists for the task and the push-auth preflight against it fails. The raw
// error is returned (not a formatted reason) so the caller can classify it as
// transient vs permanent. A not-yet-existing worktree is not a failure here —
// see the doc comment on execAdmissionPreflight.
func (e *Engine) checkAdmissionCredentials(taskID string) error {
	if e.worktrees == nil {
		return nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	return e.preflightPushCredentials(ctx, wtPath)
}

// blockAdmission flips the task to human-required with a terminal blocker
// (Exhausted: true — no automatic re-attempts) and records the decision.
func (e *Engine) blockAdmission(taskID string, step *Step, t TaskInfo, kind blocker.Kind, reason string) (StepOutput, error) {
	full := admissionPreflightReasonPrefix + " " + reason
	if err := e.tasks.UpdateTaskBlocker(taskID, taskstatus.HumanRequired, full, blocker.State{
		Kind:      kind,
		Actor:     blocker.ActorWorkflow,
		Exhausted: true,
	}); err != nil {
		return StepOutput{}, fmt.Errorf("%s: set human-required: %w", step.ID, err)
	}
	e.recordAdmissionDecision(t, AdmissionDecision{
		Outcome:        "blocked",
		RiskTier:       planContractRiskTier(t.PlanContract),
		PermissionTier: planContractPermissionTier(t.PlanContract),
		BlockerKind:    string(kind),
		Reason:         full,
	})
	e.logger.Warn("workflow.admission-preflight.blocked",
		"task_id", taskID, "step", step.ID, "kind", kind, "reason", full)
	return StepOutput{StepID: step.ID, Status: "completed", Output: full}, nil
}

// admitTask records a passing admission decision. output is either "disabled"
// (admission.Enabled=false — checks were skipped, not evaluated) or
// "admitted" (checks ran and passed); it is echoed into AdmissionDecision.Reason
// so the admission.decided audit event can tell the two apart instead of
// showing an empty reason for both.
func (e *Engine) admitTask(step *Step, t TaskInfo, output string) (StepOutput, error) {
	e.recordAdmissionDecision(t, AdmissionDecision{
		Outcome:        "admitted",
		RiskTier:       planContractRiskTier(t.PlanContract),
		PermissionTier: planContractPermissionTier(t.PlanContract),
		Reason:         output,
	})
	return StepOutput{StepID: step.ID, Status: "completed", Output: output}, nil
}

func (e *Engine) recordAdmissionDecision(t TaskInfo, d AdmissionDecision) {
	if e.admissionDecisionHook == nil {
		return
	}
	e.admissionDecisionHook(t, d)
}

// planContractRiskTier/planContractPermissionTier best-effort extract the
// contract's own risk/permission tier for the admission.decided audit event,
// so evaluation can correlate predicted risk against the actual outcome. An
// absent or malformed contract yields an empty string rather than an error —
// these are advisory audit fields, not a second validation pass.
func planContractRiskTier(raw string) string {
	contract, ok := parsePlanContractQuiet(raw)
	if !ok {
		return ""
	}
	return contract.RiskTier
}

func planContractPermissionTier(raw string) string {
	contract, ok := parsePlanContractQuiet(raw)
	if !ok {
		return ""
	}
	return contract.PermissionTier
}

func parsePlanContractQuiet(raw string) (PlanContract, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return PlanContract{}, false
	}
	contract, err := parsePlanContract(raw)
	if err != nil {
		return PlanContract{}, false
	}
	return contract, true
}
