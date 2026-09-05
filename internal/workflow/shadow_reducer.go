package workflow

import (
	"fmt"
	"strings"
	"time"
)

// shadowAdvanceSnapshot captures the inputs AdvanceStep is about to act on,
// taken before any hand-rolled write mutates them, so a deferred comparison
// can hand the *same* event to the pure Reduce function and see whether its
// predicted resting state matches what the hand-rolled path actually
// persisted. See #2750: Reduce has zero production callers today; this is
// read-only instrumentation toward proving each step type's writes are safe
// to replace with reducer effects, one step type at a time.
type shadowAdvanceSnapshot struct {
	taskID   string
	def      Definition
	task     TaskInfo
	wfExec   *Execution
	output   StepOutput
	now      time.Time
	hourly   bool
	lifetime bool
}

// snapshotForShadowAdvance clones the mutable execution state so the shadow
// comparison always reduces over the pre-write event, never a partially
// mutated in-flight copy shared with the real advance path.
func (e *Engine) snapshotForShadowAdvance(taskID string, def Definition, task TaskInfo, wfExec *Execution, output StepOutput) shadowAdvanceSnapshot {
	hourly, lifetime := e.reviewBudgetExhaustion(task)
	return shadowAdvanceSnapshot{
		taskID:   taskID,
		def:      def,
		task:     task,
		wfExec:   wfExec.Clone(),
		output:   output,
		now:      e.now(),
		hourly:   hourly,
		lifetime: lifetime,
	}
}

// shadowCompareAdvance runs Reduce over the pre-write snapshot and logs any
// divergence between its predicted resting state and what AdvanceStep's
// hand-rolled writes actually settled on. It never influences control flow —
// a Reduce error or a predicted/actual mismatch is purely informational,
// tracked per step type until that type's shadow log is clean enough to cut
// over (see #2750 acceptance criteria).
func (e *Engine) shadowCompareAdvance(snap shadowAdvanceSnapshot) {
	desired := DesiredState{
		Definition:       snap.def,
		WorkflowID:       snap.def.ID,
		ReviewUntilClean: !e.reviewLoopDisabled.Load(),
	}
	observed := ObservedState{
		Task:                   snap.task,
		Execution:              snap.wfExec,
		CompletedOutput:        &snap.output,
		Now:                    snap.now,
		ReviewBudgetExceeded:   snap.hourly,
		ReviewLifetimeExceeded: snap.lifetime,
	}

	effects, err := Reduce(desired, observed)
	if err != nil {
		e.shadowDivergence.Log(e.logger, "workflow.reducer.shadow_error",
			shadowThrottleKey(snap.taskID, snap.output.StepID), err,
			"task_id", snap.taskID, "step_id", snap.output.StepID)
		return
	}

	actual, err := e.tasks.GetTask(snap.taskID)
	if err != nil {
		// The task may have been deleted/quarantined concurrently; not a
		// reducer-relevant divergence, so drop it rather than log noise.
		return
	}

	predicted := predictedRestingState(observed.Execution, effects)
	if diff := diffShadowState(predicted, actual); diff != "" {
		e.shadowDivergence.Log(e.logger, "workflow.reducer.shadow_diverge",
			shadowThrottleKey(snap.taskID, snap.output.StepID), fmt.Errorf("%s", diff),
			"task_id", snap.taskID, "step_id", snap.output.StepID, "workflow", snap.def.ID)
	} else {
		e.shadowDivergence.Clear(shadowThrottleKey(snap.taskID, snap.output.StepID))
	}
}

func shadowThrottleKey(taskID, stepID string) string {
	return taskID + "\x00" + stepID
}

// shadowRestingState is the subset of persisted state Reduce's effects are
// expected to settle on: the workflow's resting step/state and the task's
// status. Fields the reducer does not model for a given event (e.g. no
// EffectSetTaskStatus emitted) are left zero and excluded from comparison —
// silence from the reducer is not itself a claim about that field.
type shadowRestingState struct {
	hasWorkflow  bool
	currentStep  string
	execState    ExecState
	hasStatus    bool
	status       string
	statusReason string
}

// predictedRestingState folds a reducer effect list down to the state it
// implies the engine should be resting in once dispatched, mirroring how
// AdvanceStep's hand-rolled writes settle: the last workflow-state effect
// wins (Reduce only ever emits one resting Execution per call), and a
// set_task_status effect (if any) marks the expected task status.
func predictedRestingState(before *Execution, effects []Effect) shadowRestingState {
	state := shadowRestingState{}
	if before != nil {
		state.currentStep = before.CurrentStep
		state.execState = before.State
	}
	for i := range effects {
		eff := &effects[i]
		if eff.Workflow != nil {
			state.hasWorkflow = true
			state.currentStep = eff.Workflow.CurrentStep
			state.execState = eff.Workflow.State
		}
		if eff.Kind == EffectSetTaskStatus {
			state.hasStatus = true
			state.status = eff.Status
			state.statusReason = eff.StatusReason
		}
	}
	return state
}

// diffShadowState reports a human-readable divergence between the reducer's
// prediction and the task's actual persisted state, or "" if they agree on
// every field the reducer made a claim about.
func diffShadowState(predicted shadowRestingState, actual TaskInfo) string {
	var diffs []string
	if predicted.hasWorkflow {
		var actualStep string
		var actualState ExecState
		if actual.Workflow != nil {
			actualStep = actual.Workflow.CurrentStep
			actualState = actual.Workflow.State
		}
		if predicted.currentStep != actualStep {
			diffs = append(diffs, fmt.Sprintf("current_step: predicted=%q actual=%q", predicted.currentStep, actualStep))
		}
		if predicted.execState != actualState {
			diffs = append(diffs, fmt.Sprintf("exec_state: predicted=%q actual=%q", predicted.execState, actualState))
		}
	}
	if predicted.hasStatus {
		if predicted.status != string(actual.Status) {
			diffs = append(diffs, fmt.Sprintf("status: predicted=%q actual=%q", predicted.status, actual.Status))
		}
		if predicted.statusReason != actual.StatusReason {
			diffs = append(diffs, fmt.Sprintf("status_reason: predicted=%q actual=%q", predicted.statusReason, actual.StatusReason))
		}
	}
	return strings.Join(diffs, "; ")
}
