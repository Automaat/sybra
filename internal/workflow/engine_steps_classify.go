package workflow

import (
	"context"
	"fmt"
	"time"
)

// classifyTaskTimeout bounds a single classify_task step. The classifier
// (internal/triage) makes one structured-output LLM call with a small number
// of internal repairs, so this is generous headroom, not a tight budget.
const classifyTaskTimeout = 2 * time.Minute

// execClassifyTask runs the deterministic Go triage classifier against the
// task and applies its verdict (title, tags, size/type, mode, project,
// routed status) in one atomic update. No agent session involved — this
// replaces a run_agent step that wrapped a full Sonnet agent invoking the
// /sybra-triage skill around the same classifier, which was a second LLM
// call for no benefit.
func (e *Engine) execClassifyTask(taskID string, step *Step) (StepOutput, error) {
	if e.classifier == nil {
		return e.humanRequiredClassify(taskID, step, "no task classifier configured")
	}

	ctx, cancel := context.WithTimeout(e.ctx, classifyTaskTimeout)
	defer cancel()

	if err := e.classifier.ClassifyTask(ctx, taskID); err != nil {
		return e.humanRequiredClassify(taskID, step, "triage classify failed: "+err.Error())
	}

	e.logger.Info("workflow.classify-task", "task_id", taskID, "step", step.ID)
	return StepOutput{StepID: step.ID, Status: "completed"}, nil
}

// humanRequiredClassify flips the task to human-required with reason and
// returns a completed StepOutput carrying the same reason, matching the
// pattern used throughout the other deterministic tail steps (e.g.
// humanRequiredPR, execRequireSidecar).
func (e *Engine) humanRequiredClassify(taskID string, step *Step, reason string) (StepOutput, error) {
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, fmt.Errorf("%s: set human-required: %w", step.ID, err)
	}
	e.logger.Warn("workflow.classify-task.human-required", "task_id", taskID, "step", step.ID, "reason", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
}
