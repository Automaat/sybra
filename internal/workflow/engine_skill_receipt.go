package workflow

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"time"

	"github.com/Automaat/sybra/internal/autonomy"

	"github.com/Automaat/sybra/internal/skillattr"
	"github.com/Automaat/sybra/internal/taskstatus"
)

const maxSkillReceiptRecoveryAttempts = 1

// maxSkillReceiptZeroOutputRetries bounds re-dispatches for runs that produced
// no turns at all. They are kept off the conformance budget — a child that
// emitted nothing never saw the skill — but still need a ceiling so a provider
// failing to start in a loop reaches a human instead of spinning forever.
const maxSkillReceiptZeroOutputRetries = 3

func skillReceiptRecoveryKey(stepID string) string {
	return "step." + stepID + ".skill_receipt_retry"
}

func skillReceiptZeroOutputKey(stepID string) string {
	return "step." + stepID + ".skill_receipt_zero_output"
}

func applySkillReceiptRecoveryAssignment(stepID string, wfExec *Execution, assignment *AgentAssignment) {
	if assignment == nil || wfExec == nil || stepID == "" {
		return
	}
	if parseWorkflowInt(wfExec.Variables[skillReceiptRecoveryKey(stepID)]) <= 0 {
		return
	}
	assignment.ForceInjectedSkill = true
	assignment.SkillRecoveryAttempt = true
}

// clearSkillReceiptCounters drops both receipt-retry counters once a run finally
// produces a conformance receipt, so a later unrelated miss starts from zero.
func (e *Engine) clearSkillReceiptCounters(taskID, spawnedStep string, fresh TaskInfo) {
	cleared := false
	for _, key := range []string{skillReceiptRecoveryKey(spawnedStep), skillReceiptZeroOutputKey(spawnedStep)} {
		if parseWorkflowInt(fresh.Workflow.Variables[key]) > 0 {
			delete(fresh.Workflow.Variables, key)
			cleared = true
		}
	}
	if !cleared {
		return
	}
	if err := e.tasks.SetWorkflow(taskID, fresh.Workflow); err != nil {
		e.logger.Warn("workflow.skill-receipt.clear", "task_id", taskID, "step", spawnedStep, "err", err)
	}
}

func agentRunInfoByID(runs []AgentRunInfo, agentID string) (AgentRunInfo, bool) {
	if agentID == "" {
		return AgentRunInfo{}, false
	}
	for i := range slices.Backward(runs) {
		if runs[i].AgentID == agentID {
			return runs[i], true
		}
	}
	return AgentRunInfo{}, false
}

func (e *Engine) maybeRecoverUnverifiedSkillRun(taskID, agentID, spawnedStep, output string, def *Definition, currentStep *Step) bool {
	if e.tasks == nil || def == nil || currentStep == nil {
		return false
	}
	fresh, err := e.tasks.GetTask(taskID)
	if err != nil || fresh.Workflow == nil {
		return false
	}
	run, ok := agentRunInfoByID(fresh.AgentRuns, agentID)
	if !ok {
		return false
	}
	// No turns means the child never saw the instructions, so the missing
	// receipt indicts the spawn rather than the agent's conformance.
	zeroOutput := run.TurnCount == 0
	retryKey := skillReceiptRecoveryKey(spawnedStep)
	maxAttempts := maxSkillReceiptRecoveryAttempts
	if zeroOutput {
		retryKey = skillReceiptZeroOutputKey(spawnedStep)
		maxAttempts = maxSkillReceiptZeroOutputRetries
	}
	retries := parseWorkflowInt(fresh.Workflow.Variables[retryKey])
	if run.RequestedSkill == "" {
		return false
	}
	if run.SkillConformance != skillattr.ConformanceUnverified {
		e.clearSkillReceiptCounters(taskID, spawnedStep, fresh)
		return false
	}
	if retries >= maxAttempts {
		if present, kind := importedSidecarPresentForStep(currentStep, fresh); present {
			delete(fresh.Workflow.Variables, retryKey)
			if err := e.tasks.SetWorkflow(taskID, fresh.Workflow); err != nil {
				e.logger.Warn("workflow.skill-receipt.sidecar-present.persist", "task_id", taskID, "step", spawnedStep, "err", err)
			}
			e.logger.Warn("workflow.skill-receipt.sidecar-present",
				"task_id", taskID, "step", spawnedStep, "skill", run.RequestedSkill, "sidecar", kind)
			return false
		}
		delete(fresh.Workflow.Variables, retryKey)
		// Finalize the Execution like finishTerminalStepOutput does for every
		// other human-required escalation (e.g. checkpoint_failed). Left
		// non-terminal, StartWorkflow/DispatchEvent's active-workflow guard
		// rejects the fresh "testing" trigger a human-review recovery issues,
		// so only ResumeStalled's accidental re-dispatch against this same
		// stale Execution can move the task — racing a concurrent recovery
		// attempt and letting a later genuine PASS still land on a task
		// re-parked at human-required by an unrelated stale completion.
		now := time.Now()
		fresh.Workflow.CurrentStep = ""
		fresh.Workflow.State = ExecCompleted
		fresh.Workflow.CompletedAt = &now
		summary := skillReceiptExhaustionSummary(output)
		reason := fmt.Sprintf("mandatory workflow skill %q produced no conformance receipt after automatic recovery retry", run.RequestedSkill)
		if zeroOutput {
			reason = fmt.Sprintf("provider produced no output across %d attempts, so mandatory workflow skill %q never ran", maxAttempts+1, run.RequestedSkill)
		}
		if summary != "" {
			reason += ": " + summary
		}
		escalation := autonomy.NewEscalation("workflow.skill_receipt_exhausted", autonomy.FailureOwnerMachine, autonomy.ProvenanceControlPlane, reason)
		if err := e.tasks.SetEscalationAndWorkflow(taskID, string(taskstatus.Blocked), reason, escalation, autonomy.OutcomeQuarantined, fresh.Workflow); err != nil {
			e.logger.Error("workflow.skill-receipt.human-required", "task_id", taskID, "step", spawnedStep, "err", err)
		}
		e.logger.Warn("workflow.skill-receipt.exhausted", "task_id", taskID, "step", spawnedStep, "skill", run.RequestedSkill, "zero_output", zeroOutput, "summary", summary)
		e.fireComplete(&CompletionInfo{
			TaskID:     taskID,
			WorkflowID: fresh.Workflow.WorkflowID,
			Variables:  maps.Clone(fresh.Workflow.Variables),
		})
		return true
	}
	e.captureSkillReceiptDiagnostics(taskID, spawnedStep, currentStep, fresh)
	fresh.Workflow.SetVar(retryKey, strconv.Itoa(retries+1))
	if err := e.tasks.SetWorkflow(taskID, fresh.Workflow); err != nil {
		e.logger.Error("workflow.skill-receipt.persist", "task_id", taskID, "step", spawnedStep, "err", err)
		return true
	}
	switch {
	case currentStep.Type == StepRunAgent && currentStep.ID == spawnedStep:
		e.logger.Warn("workflow.skill-receipt.retry", "task_id", taskID, "step", spawnedStep, "skill", run.RequestedSkill, "zero_output", zeroOutput)
		e.clearAgentStep(taskID, agentID)
		clearAgentRouteFromWorkflow(fresh.Workflow, agentID)
		e.rescheduleRunAgent(taskID, agentID, currentStep, fresh, def, "workflow.skill-receipt-reschedule", nil)
		return true
	case currentStep.Type == StepParallel && parallelHasChild(currentStep, spawnedStep):
		child := def.StepByID(spawnedStep)
		if child == nil || child.Type != StepRunAgent {
			return false
		}
		e.logger.Warn("workflow.skill-receipt.retry", "task_id", taskID, "parent", currentStep.ID, "child", spawnedStep, "skill", run.RequestedSkill, "zero_output", zeroOutput)
		e.clearAgentStep(taskID, agentID)
		clearAgentRouteFromWorkflow(fresh.Workflow, agentID)
		e.rescheduleSkillReceiptParallelChild(taskID, currentStep, child)
		return true
	default:
		return false
	}
}

func importedSidecarPresentForStep(step *Step, t TaskInfo) (present bool, kind string) {
	if step == nil {
		return false, ""
	}
	for _, cfg := range step.Config.sidecarImports() {
		sidecarKind := cfg.Kind
		if sidecarKind == "plan_draft" {
			sidecarKind = "plan_draft." + step.ID
		}
		if strings.TrimSpace(taskSidecarContent(t, sidecarKind)) != "" {
			return true, sidecarKind
		}
	}
	return false, ""
}

func taskSidecarContent(t TaskInfo, kind string) string {
	switch {
	case kind == "plan":
		return t.Plan
	case kind == "plan_contract":
		return t.PlanContract
	case kind == "plan_critique":
		return t.PlanCritique
	case kind == "plan_research":
		return t.PlanResearch
	case kind == "plan_decisions":
		return t.PlanDecisions
	case kind == "plan_brief":
		return t.PlanBrief
	case kind == "code_review":
		return t.CodeReview
	case strings.HasPrefix(kind, "plan_draft."):
		return t.PlanDrafts[strings.TrimPrefix(kind, "plan_draft.")]
	default:
		return ""
	}
}

func (e *Engine) rescheduleSkillReceiptParallelChild(taskID string, parent, child *Step) {
	unlockInflight := e.acquireInflight(taskID)
	defer unlockInflight()

	fresh, err := e.tasks.GetTask(taskID)
	_, skip := resumeSkipReasonForStatus(fresh.Status)
	if err != nil || fresh.Workflow == nil || fresh.Workflow.CurrentStep != parent.ID || fresh.Workflow.State == ExecCompleted || fresh.Workflow.State == ExecFailed || skip {
		return
	}
	wfExec := fresh.Workflow
	rec := wfExec.ParallelInflight[parent.ID]
	if rec == nil {
		return
	}
	status := rec.Children[child.ID]
	if status == nil {
		return
	}
	status.Status = "pending"
	status.Output = "missing skill receipt: retrying with injected skill instructions"
	status.AgentID = ""

	fresh = e.withManualTestConfig(fresh)
	ctx := TemplateContext{
		Task:     fresh,
		Step:     *parent,
		Vars:     wfExec.Variables,
		Workflow: wfExec,
	}
	dir := wfExec.Variables[WorkflowVarDir]
	spawnErr := e.spawnParallelChild(taskID, parent, child, wfExec, ctx, dir, status)
	if spawnErr != nil {
		status.Status = "pending"
		status.Output = "reschedule failed: " + spawnErr.Error()
		status.AgentID = ""
	}
	if setErr := e.tasks.SetWorkflow(taskID, wfExec); setErr != nil {
		e.logger.Error("workflow.skill-receipt.parallel.set", "task_id", taskID, "parent", parent.ID, "child", child.ID, "err", setErr)
	}
	if spawnErr != nil {
		e.surfaceStartFailure(taskID, fresh.Status, spawnErr, wfExec, child.ID)
	}
}

func (e *Engine) captureSkillReceiptDiagnostics(taskID, spawnedStep string, currentStep *Step, t TaskInfo) {
	if e.recorder == nil || currentStep == nil {
		return
	}
	var step *Step
	switch {
	case currentStep.Type == StepRunAgent && currentStep.ID == spawnedStep:
		step = currentStep
	case currentStep.Type == StepParallel && parallelHasChild(currentStep, spawnedStep):
		for i := range currentStep.Parallel {
			if currentStep.Parallel[i].ID == spawnedStep {
				step = &currentStep.Parallel[i]
				break
			}
		}
	}
	if step == nil {
		return
	}
	for _, cfg := range step.Config.sidecarImports() {
		name, content, ok := skillReceiptDiagnosticSidecar(t, spawnedStep, cfg.Kind)
		if !ok {
			continue
		}
		if err := e.recorder.PutGeneric(taskID, name, spawnedStep, content); err != nil {
			e.logger.Warn("artifact.record.failed", "kind", "skill_receipt_diagnostic", "task_id", taskID, "step", spawnedStep, "err", err)
		}
	}
}

func skillReceiptDiagnosticSidecar(t TaskInfo, stepID, kind string) (name, content string, ok bool) {
	label := strings.ReplaceAll(kind, "_", "-")
	switch {
	case kind == "plan":
		content = t.Plan
	case kind == "plan_contract":
		content = t.PlanContract
	case kind == "plan_critique":
		content = t.PlanCritique
	case kind == "plan_research":
		content = t.PlanResearch
	case kind == "plan_decisions":
		content = t.PlanDecisions
	case kind == "plan_brief":
		content = t.PlanBrief
	case kind == "code_review":
		content = t.CodeReview
	case kind == "plan_draft":
		content = t.PlanDrafts[stepID]
	case strings.HasPrefix(kind, "plan_draft."):
		content = t.PlanDrafts[strings.TrimPrefix(kind, "plan_draft.")]
	default:
		return "", "", false
	}
	if strings.TrimSpace(content) == "" {
		return "", "", false
	}
	ext := ".md"
	if kind == "plan_contract" {
		ext = ".json"
	}
	return "skill-receipt-first-" + stepID + "-" + label + ext, content, true
}
