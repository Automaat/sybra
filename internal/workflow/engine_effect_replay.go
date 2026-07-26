package workflow

import (
	"cmp"
	"context"
	"slices"

	"github.com/Automaat/sybra/internal/dispatchorder"
	"github.com/Automaat/sybra/internal/metrics"
)

func (e *Engine) metricContext() context.Context {
	if e.ctx != nil {
		return e.ctx
	}
	return context.Background()
}

// ReplayPersistedEffects re-enters the current workflow step when a persisted
// step-effect intent exists without a matching completion. Completed records are
// reconciled as a no-op so restart recovery can prefer durable effect state
// over free-form task-status inference.
func (e *Engine) ReplayPersistedEffects() {
	if e.dispatchDisabled.Load() {
		return
	}

	tasks, err := e.tasks.ListTasks()
	if err != nil {
		e.logger.Error("workflow.effect-replay.list", "err", err)
		return
	}

	if e.dispatchComparator != nil {
		slices.SortStableFunc(tasks, e.dispatchComparator())
	} else {
		slices.SortStableFunc(tasks, func(a, b TaskInfo) int {
			return cmp.Compare(dispatchorder.Rank(a.Status), dispatchorder.Rank(b.Status))
		})
	}

	for i := range tasks {
		e.replayPersistedEffectsTask(&tasks[i])
	}
}

func (e *Engine) replayPersistedEffectsTask(t *TaskInfo) {
	if t.Workflow == nil || t.Workflow.CurrentStep == "" {
		return
	}
	if e.dispatchGate != nil && !e.dispatchGate(*t) {
		return
	}
	switch t.Workflow.State {
	case ExecCompleted, ExecFailed:
		return
	case ExecRunning, ExecWaiting:
	default:
		return
	}
	if e.agents.HasRunningAgent(t.ID) || e.agents.IsDispatching(t.ID) {
		return
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		return
	}
	rec, ok := currentStepEffectRecord(*t, step, effectPosStepAction)
	if !ok {
		return
	}
	if rec.CompletedAt == nil && e.resumePreflightConsumesTick(t, step, "workflow.effect-replay.skip") {
		return
	}
	e.replayPendingEffect(t, step, &def)
}

func (e *Engine) replayPendingEffect(t *TaskInfo, step *Step, def *Definition) {
	reason, acquired := e.tryMarkResumeDispatching(t.ID, step)
	if !acquired {
		e.resumeSkip.Log(e.logger, "workflow.effect-replay.skip", t.ID,
			reason+"|"+step.ID,
			"task_id", t.ID, "reason", reason, "step", step.ID)
		return
	}

	fresh, abort := e.resolveFreshTaskForResume(t, step, def)
	if abort {
		return
	}
	if e.agents.HasRunningAgent(fresh.ID) || e.agents.IsDispatching(fresh.ID) {
		e.clearResumeDispatching(fresh.ID)
		return
	}
	if e.dispatchGate != nil && !e.dispatchGate(fresh) {
		e.clearResumeDispatching(t.ID)
		return
	}

	var err error
	*def, err = e.store.Get(fresh.Workflow.WorkflowID)
	if err != nil {
		e.clearResumeDispatching(t.ID)
		return
	}
	step = def.StepByID(fresh.Workflow.CurrentStep)
	if step == nil {
		e.clearResumeDispatching(t.ID)
		return
	}
	rec, ok := currentStepEffectRecord(fresh, step, effectPosStepAction)
	if !ok {
		e.clearResumeDispatching(t.ID)
		return
	}
	if rec.CompletedAt != nil {
		e.clearResumeDispatching(t.ID)
		e.logger.Info("workflow.effect-replay.noop",
			"task_id", fresh.ID, "step", step.ID, "effect", rec.ID.String())
		metrics.OrchestratorEffectReplay(e.metricContext(), "already_completed")
		return
	}
	if e.effectReplayConsumedRetry(&fresh, step, t.ID) {
		return
	}

	e.logger.Info("workflow.effect-replay", "task_id", fresh.ID, "step", step.ID, "effect", rec.ID.String())
	comp, rErr := e.executeSteps(fresh.ID, def, step, fresh.Workflow)
	rErr = normalizeExecuteStepsErr(rErr)
	e.clearResumeDispatching(fresh.ID)
	e.fireComplete(comp)
	e.drainPendingConflictRecovery(fresh.ID)
	e.resumeError.Log(e.logger, "workflow.effect-replay.exec", fresh.ID, rErr, "task_id", fresh.ID)
	if rErr != nil {
		metrics.OrchestratorEffectReplay(e.metricContext(), "error")
		e.surfaceStartFailure(fresh.ID, fresh.Status, rErr, fresh.Workflow, step.ID)
		return
	}

	metrics.OrchestratorEffectReplay(e.metricContext(), "replayed")
	cleanupWorkflow := e.workflowForPostDispatchCleanup(fresh.ID)
	e.clearTransientFetchRetry(fresh.ID, cleanupWorkflow, step.ID)
	e.clearCircuitBreakerOnSuccess(fresh.ID, cleanupWorkflow, step.ID)
	e.clearWatchdogReaskNote(fresh.ID, cleanupWorkflow)
}

func (e *Engine) effectReplayConsumedRetry(t *TaskInfo, step *Step, claimTaskID string) bool {
	if e.handleTransientFetchRetry(t, step) {
		e.clearResumeDispatching(claimTaskID)
		return true
	}
	if e.handleWatchdogRateLimitRetry(t, step) {
		e.clearResumeDispatching(claimTaskID)
		return true
	}
	return false
}
