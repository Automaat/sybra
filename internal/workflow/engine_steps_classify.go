package workflow

import (
	"context"
	"errors"
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
func (e *Engine) execClassifyTask(taskID string, step *Step, wfExec *Execution) (StepOutput, error) {
	if e.classifier == nil {
		return e.humanRequiredClassify(taskID, step, "no task classifier configured")
	}

	ctx, cancel := context.WithTimeout(e.ctx, classifyTaskTimeout)
	defer cancel()

	// Restore the retry budget the replaced run_agent step carried
	// (max_retries: 3). A transient rate limit or brief provider outage now
	// retries with backoff instead of permanently parking the task on
	// human-required on the first blip.
	var err error
	for attempt := 0; ; attempt++ {
		if err = e.classifier.ClassifyTask(ctx, taskID); err == nil {
			e.logger.Info("workflow.classify-task", "task_id", taskID, "step", step.ID)
			return StepOutput{StepID: step.ID, Status: "completed"}, nil
		}
		// Engine shutdown (parent context canceled), not a classify failure —
		// park the workflow WITHOUT recording/advancing CurrentStep, so the
		// step re-runs on next boot instead of a "completed" record baking
		// "context canceled" into a permanent skip (recording completed here
		// would let resolveNext advance past triage without it ever having
		// run). ResumeStalled's step-type allowlist includes classify_task
		// specifically so this park is actually picked back up, not stranded.
		// The per-step deadline (classifyTaskTimeout) fires on the derived
		// ctx, not e.ctx, so a genuinely hung classify still escalates below.
		if errors.Is(e.ctx.Err(), context.Canceled) || errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
			e.logger.Warn("workflow.classify-task.canceled", "task_id", taskID, "step", step.ID, "err", err)
			wfExec.State = ExecWaiting
			if setErr := e.tasks.SetWorkflow(taskID, wfExec); setErr != nil {
				return StepOutput{}, setErr
			}
			return StepOutput{}, errStepParked
		}
		if attempt >= len(classifyTaskRetryBackoffs) {
			break
		}
		e.logger.Warn("workflow.classify-task.retry", "task_id", taskID, "step", step.ID,
			"attempt", attempt+1, "max", len(classifyTaskRetryBackoffs), "err", err)
		// Interruptible backoff: an engine shutdown (parent ctx) or the per-step
		// deadline cancels ctx and wakes the wait immediately, so the retry loop
		// never blocks teardown. The next iteration's e.ctx check then routes a
		// shutdown to the resume-on-next-boot path above.
		classifyTaskWait(ctx, classifyTaskRetryBackoffs[attempt])
	}

	return e.humanRequiredClassify(taskID, step, "triage classify failed: "+err.Error())
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
