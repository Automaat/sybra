package workflow

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/skillattr"
)

const maxSkillReceiptRecoveryAttempts = 1

func skillReceiptRecoveryKey(stepID string) string {
	return "step." + stepID + ".skill_receipt_retry"
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

func (e *Engine) maybeRecoverUnverifiedSkillRun(taskID, agentID, spawnedStep string, def *Definition, currentStep *Step) bool {
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
	retryKey := skillReceiptRecoveryKey(spawnedStep)
	retries := parseWorkflowInt(fresh.Workflow.Variables[retryKey])
	if run.RequestedSkill == "" {
		return false
	}
	if run.SkillConformance != skillattr.ConformanceUnverified {
		if retries > 0 {
			delete(fresh.Workflow.Variables, retryKey)
			if err := e.tasks.SetWorkflow(taskID, fresh.Workflow); err != nil {
				e.logger.Warn("workflow.skill-receipt.clear", "task_id", taskID, "step", spawnedStep, "err", err)
			}
		}
		return false
	}
	if retries >= maxSkillReceiptRecoveryAttempts {
		delete(fresh.Workflow.Variables, retryKey)
		if err := e.tasks.SetWorkflow(taskID, fresh.Workflow); err != nil {
			e.logger.Warn("workflow.skill-receipt.clear", "task_id", taskID, "step", spawnedStep, "err", err)
		}
		reason := fmt.Sprintf("mandatory workflow skill %q produced no conformance receipt after automatic recovery retry", run.RequestedSkill)
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.skill-receipt.human-required", "task_id", taskID, "step", spawnedStep, "err", statusErr)
		}
		e.logger.Warn("workflow.skill-receipt.exhausted", "task_id", taskID, "step", spawnedStep, "skill", run.RequestedSkill)
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
		e.logger.Warn("workflow.skill-receipt.retry", "task_id", taskID, "step", spawnedStep, "skill", run.RequestedSkill)
		e.clearAgentStep(agentID)
		e.rescheduleRunAgent(taskID, agentID, currentStep, fresh, def, "workflow.skill-receipt-reschedule", nil)
		return true
	case currentStep.Type == StepParallel && parallelHasChild(currentStep, spawnedStep):
		child := def.StepByID(spawnedStep)
		if child == nil || child.Type != StepRunAgent {
			return false
		}
		e.logger.Warn("workflow.skill-receipt.retry", "task_id", taskID, "parent", currentStep.ID, "child", spawnedStep, "skill", run.RequestedSkill)
		e.clearAgentStep(agentID)
		e.rescheduleSkillReceiptParallelChild(taskID, currentStep, child)
		return true
	default:
		return false
	}
}

func (e *Engine) rescheduleSkillReceiptParallelChild(taskID string, parent, child *Step) {
	e.acquireInflight(taskID)
	defer e.releaseInflight(taskID)

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
