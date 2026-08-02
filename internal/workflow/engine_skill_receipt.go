package workflow

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

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
		if e.importedSidecarSatisfiesReceipt(fresh, spawnedStep, stepForSpawned(currentStep, spawnedStep)) {
			// Recovery is exhausted, but the mandatory sidecar this skill
			// exists to produce (e.g. adversarial-review's code_review) was
			// already imported onto the task. Consume that valid artifact
			// instead of escalating on the missing conformance receipt alone:
			// clear the retry counter and return false so the normal advance
			// path proceeds.
			delete(fresh.Workflow.Variables, retryKey)
			if err := e.tasks.SetWorkflow(taskID, fresh.Workflow); err != nil {
				e.logger.Warn("workflow.skill-receipt.clear", "task_id", taskID, "step", spawnedStep, "err", err)
			}
			e.logger.Info("workflow.skill-receipt.sidecar-satisfied", "task_id", taskID, "step", spawnedStep, "skill", run.RequestedSkill)
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
		if err := e.tasks.SetWorkflow(taskID, fresh.Workflow); err != nil {
			e.logger.Warn("workflow.skill-receipt.clear", "task_id", taskID, "step", spawnedStep, "err", err)
		}
		summary := skillReceiptExhaustionSummary(output)
		reason := fmt.Sprintf("mandatory workflow skill %q produced no conformance receipt after automatic recovery retry", run.RequestedSkill)
		if summary != "" {
			reason += ": " + summary
		}
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.skill-receipt.human-required", "task_id", taskID, "step", spawnedStep, "err", statusErr)
		}
		e.logger.Warn("workflow.skill-receipt.exhausted", "task_id", taskID, "step", spawnedStep, "skill", run.RequestedSkill, "summary", summary)
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
		e.logger.Warn("workflow.skill-receipt.retry", "task_id", taskID, "step", spawnedStep, "skill", run.RequestedSkill)
		e.clearAgentStep(taskID, agentID)
		clearAgentRouteFromWorkflow(fresh.Workflow, agentID)
		e.rescheduleRunAgent(taskID, agentID, currentStep, fresh, def, "workflow.skill-receipt-reschedule", nil)
		return true
	case currentStep.Type == StepParallel && parallelHasChild(currentStep, spawnedStep):
		child := def.StepByID(spawnedStep)
		if child == nil || child.Type != StepRunAgent {
			return false
		}
		e.logger.Warn("workflow.skill-receipt.retry", "task_id", taskID, "parent", currentStep.ID, "child", spawnedStep, "skill", run.RequestedSkill)
		e.clearAgentStep(taskID, agentID)
		clearAgentRouteFromWorkflow(fresh.Workflow, agentID)
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

// stepForSpawned resolves the concrete run_agent step (currentStep itself, or
// the matching parallel child) that spawnedStep refers to. Returns nil when
// spawnedStep does not name currentStep or one of its children.
func stepForSpawned(currentStep *Step, spawnedStep string) *Step {
	if currentStep == nil {
		return nil
	}
	switch {
	case currentStep.Type == StepRunAgent && currentStep.ID == spawnedStep:
		return currentStep
	case currentStep.Type == StepParallel && parallelHasChild(currentStep, spawnedStep):
		for i := range currentStep.Parallel {
			if currentStep.Parallel[i].ID == spawnedStep {
				return &currentStep.Parallel[i]
			}
		}
	}
	return nil
}

// importedSidecarSatisfiesReceipt reports whether every sidecar the step that
// produced this run declares was actually produced *by this run* — i.e. its
// source file exists and is non-empty in the worktree the run used. When it is,
// a missing conformance receipt is not fatal: the mandatory artifact the skill
// exists to produce is present, so the workflow can consume it rather than
// escalate to human-required on the receipt alone.
//
// It deliberately checks the on-disk sidecar file (cfg.From) rather than the
// persisted task field (e.g. t.CodeReview). That field is flat and unscoped: it
// survives across review rounds and is only overwritten on a *successful*
// import. The review-fix loop's clear_review_sidecar deletes the worktree file
// between rounds but never clears t.CodeReview, so a round-2 reviewer that fails
// to invoke the skill would otherwise be treated as satisfied by round 1's
// stale verdict — silently shipping the round-2 diff unreviewed and skipping the
// human escalation this exhaustion path exists to trigger. Keying on the file
// this run produced ties satisfaction to the actual run.
func (e *Engine) importedSidecarSatisfiesReceipt(t TaskInfo, spawnedStep string, step *Step) bool {
	if step == nil || t.Workflow == nil {
		return false
	}
	imports := step.Config.sidecarImports()
	if len(imports) == 0 {
		return false
	}
	for _, cfg := range imports {
		if !e.sidecarFilePresentForRun(t, spawnedStep, step, cfg) {
			return false
		}
	}
	return true
}

// sidecarFilePresentForRun reports whether cfg.From (rendered against the
// worktree this run used) exists and is non-empty right now. Mirrors
// importOneSidecar's read path — including the lost-_dir recovery via
// recoverSidecarFromTaskWorktree — so the presence check agrees with what a
// fresh import would actually find.
func (e *Engine) sidecarFilePresentForRun(t TaskInfo, stepID string, step *Step, cfg ImportSidecar) bool {
	path, err := RenderTemplate(cfg.From, TemplateContext{
		Task:     t,
		Step:     *step,
		Vars:     t.Workflow.Variables,
		Workflow: t.Workflow,
	})
	if err != nil {
		return false
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		dirVarUnresolved := worktreeDirTemplatePattern.MatchString(cfg.From) && strings.TrimSpace(t.Workflow.Variables[WorkflowVarDir]) == ""
		if dirVarUnresolved {
			if _, recoveredContent, ok := e.recoverSidecarFromTaskWorktree(t.ID, stepID, step, t, cfg); ok {
				content, readErr = recoveredContent, nil
			}
		}
		if readErr != nil {
			return false
		}
	}
	return strings.TrimSpace(string(content)) != ""
}

func (e *Engine) captureSkillReceiptDiagnostics(taskID, spawnedStep string, currentStep *Step, t TaskInfo) {
	if e.recorder == nil || currentStep == nil {
		return
	}
	step := stepForSpawned(currentStep, spawnedStep)
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
