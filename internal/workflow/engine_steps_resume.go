package workflow

import (
	"encoding/json"
	"time"
)

// resumeWorkflowIDVar/resumeWorkflowVarsVar/resumeStatusVar are the vars a
// recovery workflow (branch-conflict-fix) is seeded with before it starts,
// captured from the task's active workflow (if any) before it was cancelled.
// resumeStatusVar is always set (to the task's status at the moment recovery
// began) so a task that had no active workflow still gets its visible status
// restored before the normal status-driven cascade (see
// completion.Handler.OnWorkflowComplete) picks up wherever it left off.
const (
	resumeWorkflowIDVar   = "resume_workflow_id"
	resumeWorkflowStepVar = "resume_workflow_step"
	resumeWorkflowVarsVar = "resume_workflow_vars"
	resumeStatusVar       = "resume_status"
	resumeStatusReasonVar = "resume_status_reason"
)

// execResumeWorkflow is the terminal step of a recovery workflow: it restores
// the task's pre-recovery status and, when a specific workflow was captured
// before recovery began, re-enters it directly via StartWorkflowWithVars —
// this is how a branch-conflict recovery resumes the exact interrupted
// stage (review/testing/etc.) instead of skipping to a terminal status.
//
// When no workflow was captured (the task had no active workflow at the
// time recovery began), this is a no-op beyond restoring status: the step
// completes normally, the recovery workflow ends, and
// completion.Handler.OnWorkflowComplete's existing status-driven cascade
// dispatches whatever workflow matches the now-restored task.status — the
// same mechanism every other builtin workflow chain (plan → implement →
// review → ...) already relies on.
//
// Double-dispatch note: this step is not a run_agent step, so
// ResumeStalled/tryMarkResumeDispatching never targets it — it only ever
// runs once, synchronously, as part of executeSteps for the recovery
// workflow's own single pass. No separate re-dispatch path exists for a
// resume_workflow step, so no additional claim/lock is needed here.
func (e *Engine) execResumeWorkflow(taskID string, step *Step, wfExec *Execution) (StepOutput, error) {
	if status := wfExec.Variables[resumeStatusVar]; status != "" {
		if err := e.tasks.UpdateTaskStatus(taskID, status, wfExec.Variables[resumeStatusReasonVar]); err != nil {
			e.logger.Warn("workflow.resume.restore-status", "task_id", taskID, "status", status, "err", err)
		}
	}

	resumeID := wfExec.Variables[resumeWorkflowIDVar]
	if resumeID == "" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "no resume target"}, nil
	}
	resumeStep := wfExec.Variables[resumeWorkflowStepVar]

	var resumeVars map[string]string
	if raw := wfExec.Variables[resumeWorkflowVarsVar]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &resumeVars); err != nil {
			e.logger.Warn("workflow.resume.vars-decode", "task_id", taskID, "err", err)
		}
	}

	// StartWorkflowWithVars refuses to start a new workflow while the task's
	// current workflow (this one) is non-terminal, so finalize THIS execution
	// first and persist it before launching the resume target. Returning
	// errStepParked afterward tells executeSteps to stop without its own
	// record/resolveNext/SetWorkflow pass — which would otherwise re-persist
	// this now-stale (but already-completed) Execution over the freshly
	// started resume workflow's own state.
	now := time.Now().UTC()
	wfExec.State = ExecCompleted
	wfExec.CompletedAt = &now
	wfExec.CurrentStep = ""
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return StepOutput{}, err
	}

	if err := e.StartWorkflowFromStepWithVars(taskID, resumeID, resumeStep, resumeVars); err != nil {
		// Not fatal to the recovery itself — the branch conflict is already
		// resolved and pushed. Leave the task at its restored status; a human
		// or the next monitor pass can re-drive it.
		e.logger.Error("workflow.resume.start-failed",
			"task_id", taskID, "resume_workflow", resumeID, "resume_step", resumeStep, "err", err)
		return StepOutput{}, errStepParked
	}
	e.logger.Info("workflow.resume.started",
		"task_id", taskID, "resume_workflow", resumeID, "resume_step", resumeStep)
	return StepOutput{}, errStepParked
}
