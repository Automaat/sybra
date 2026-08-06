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
			return cmp.Compare(dispatchorder.Rank(string(a.Status)), dispatchorder.Rank(string(b.Status)))
		})
	}

	for i := range tasks {
		e.replayPersistedEffectsTask(&tasks[i])
	}
}

// ReplayPersistedEffectsForTask replays or reconciles the persisted current-step
// effect for one task. It reports whether durable effect state consumed the
// caller's recovery tick, even if the effect resolved as a no-op or was fenced
// by an existing dispatcher.
func (e *Engine) ReplayPersistedEffectsForTask(taskID string) bool {
	if e.dispatchDisabled.Load() {
		return false
	}
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return false
	}
	return e.replayPersistedEffectsTask(&t)
}

func (e *Engine) replayPersistedEffectsTask(t *TaskInfo) bool {
	if t.Workflow == nil || t.Workflow.CurrentStep == "" {
		return false
	}
	if e.dispatchGate != nil && !e.dispatchGate(*t) {
		return false
	}
	switch t.Workflow.State {
	case ExecCompleted, ExecFailed:
		return false
	case ExecRunning, ExecWaiting:
	default:
		return false
	}
	if e.agents.HasRunningAgent(t.ID) || e.agents.IsDispatching(t.ID) {
		return false
	}

	def, err := e.store.Get(t.Workflow.WorkflowID)
	if err != nil {
		return false
	}
	step := def.StepByID(t.Workflow.CurrentStep)
	if step == nil {
		return false
	}
	rec, ok := currentStepEffectRecord(*t, step, effectPosStepAction)
	if !ok {
		return false
	}
	if rec.CompletedAt == nil && e.resumePreflightConsumesTick(t, step, "workflow.effect-replay.skip") {
		return true
	}
	return e.replayPendingEffect(t, step, &def)
}

func (e *Engine) replayPendingEffect(t *TaskInfo, step *Step, def *Definition) bool {
	reason, acquired := e.tryMarkResumeDispatching(t.ID, step)
	if !acquired {
		e.resumeSkip.Log(e.logger, "workflow.effect-replay.skip", t.ID,
			reason+"|"+step.ID,
			"task_id", t.ID, "reason", reason, "step", step.ID)
		return true
	}

	fresh, abort := e.resolveFreshTaskForResume(t, step, def)
	if abort {
		return true
	}
	if e.agents.HasRunningAgent(fresh.ID) || e.agents.IsDispatching(fresh.ID) {
		e.clearResumeDispatching(fresh.ID)
		return true
	}
	if e.dispatchGate != nil && !e.dispatchGate(fresh) {
		e.clearResumeDispatching(t.ID)
		return true
	}

	var err error
	*def, err = e.store.Get(fresh.Workflow.WorkflowID)
	if err != nil {
		e.clearResumeDispatching(t.ID)
		return true
	}
	step = def.StepByID(fresh.Workflow.CurrentStep)
	if step == nil {
		e.clearResumeDispatching(t.ID)
		return true
	}
	rec, ok := currentStepEffectRecord(fresh, step, effectPosStepAction)
	if !ok {
		e.clearResumeDispatching(t.ID)
		return true
	}
	if rec.CompletedAt != nil {
		e.clearResumeDispatching(t.ID)
		e.logger.Info("workflow.effect-replay.noop",
			"task_id", fresh.ID, "step", step.ID, "effect", rec.ID.String())
		metrics.OrchestratorEffectReplay(e.metricContext(), "already_completed")
		return true
	}
	if e.effectReplayConsumedRetry(&fresh, step, t.ID) {
		return true
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
		return true
	}

	metrics.OrchestratorEffectReplay(e.metricContext(), "replayed")
	cleanupWorkflow := e.workflowForPostDispatchCleanup(fresh.ID)
	e.clearTransientFetchRetry(fresh.ID, cleanupWorkflow, step.ID)
	e.clearCircuitBreakerOnSuccess(fresh.ID, cleanupWorkflow, step.ID)
	e.clearDeliveredWatchdogReaskNote(fresh.ID, step, cleanupWorkflow)
	return true
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
