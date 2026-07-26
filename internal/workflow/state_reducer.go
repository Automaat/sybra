package workflow

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DesiredState is the reducer's intended workflow definition/config snapshot.
type DesiredState struct {
	Definition       Definition
	WorkflowID       string
	StartStep        string
	DesiredVars      map[string]string
	ReviewUntilClean bool
	TransitionExtras map[string]string
}

// ObservedState is the reducer's caller-supplied runtime snapshot.
type ObservedState struct {
	Task                 TaskInfo
	Execution            *Execution
	CompletedOutput      *StepOutput
	Now                  time.Time
	ReviewBudgetExceeded bool
}

type EffectKind string

const (
	EffectSetWorkflowState EffectKind = "set_workflow_state"
	EffectSetTaskStatus    EffectKind = "set_task_status"
	EffectRecordStep       EffectKind = "record_step"
	EffectDispatchStep     EffectKind = "dispatch_step"
	EffectWaitHuman        EffectKind = "wait_human"
	EffectCompleteWorkflow EffectKind = "complete_workflow"
	EffectFailWorkflow     EffectKind = "fail_workflow"
)

// Effect is a plain-data reducer output.
type Effect struct {
	Kind         EffectKind
	Workflow     *Execution
	Status       string
	StatusReason string
	Record       *StepRecord
	Step         *Step
	HumanActions []string
}

// Reduce converts desired/observed workflow state into plain-data effects.
func Reduce(desired DesiredState, observed ObservedState) ([]Effect, error) {
	workflowID, err := reducerWorkflowID(desired)
	if err != nil {
		return nil, err
	}
	if observed.Now.IsZero() {
		return nil, fmt.Errorf("malformed reducer input: observed time is required")
	}
	if observed.Execution == nil {
		if observed.CompletedOutput != nil {
			return nil, fmt.Errorf("malformed reducer input: completed output requires an execution")
		}
		start, err := reducerStartStep(desired)
		if err != nil {
			return nil, err
		}
		exec := &Execution{
			WorkflowID:  workflowID,
			CurrentStep: start.ID,
			State:       ExecRunning,
			Variables:   maps.Clone(desired.DesiredVars),
			StartedAt:   observed.Now,
		}
		if reducerParksExistingExecution(start) {
			return reduceCurrentStep(desired, observed, exec, start, []Effect{newWorkflowEffect(EffectSetWorkflowState, exec)})
		}
		return []Effect{
			newWorkflowEffect(EffectSetWorkflowState, exec),
			{Kind: EffectDispatchStep, Step: cloneStep(start)},
		}, nil
	}

	exec := observed.Execution.Clone()
	if exec == nil {
		return nil, fmt.Errorf("malformed reducer input: execution clone is nil")
	}
	if exec.WorkflowID != "" && exec.WorkflowID != workflowID {
		return nil, fmt.Errorf("malformed reducer input: execution workflow %q does not match desired workflow %q", exec.WorkflowID, workflowID)
	}
	if exec.WorkflowID == "" {
		exec.WorkflowID = workflowID
	}
	if strings.TrimSpace(exec.CurrentStep) == "" {
		return nil, fmt.Errorf("missing current step for workflow %s", workflowID)
	}
	current := desired.Definition.StepByID(exec.CurrentStep)
	if current == nil {
		return nil, fmt.Errorf("missing current step %s in workflow %s", exec.CurrentStep, workflowID)
	}

	if observed.CompletedOutput != nil {
		return reduceCompletedStep(desired, observed, exec, current)
	}

	if effects, ok, err := previewWaitHumanByStatus(desired, observed, exec, current); ok || err != nil {
		return effects, err
	}
	return reduceCurrentStep(desired, observed, exec, current, nil)
}

func reduceCompletedStep(desired DesiredState, observed ObservedState, exec *Execution, current *Step) ([]Effect, error) {
	output := *observed.CompletedOutput
	if strings.TrimSpace(output.StepID) == "" {
		return nil, fmt.Errorf("malformed reducer input: completed output step id is required")
	}
	if output.StepID != current.ID {
		return nil, fmt.Errorf("malformed reducer input: completed output step %q does not match current step %q", output.StepID, current.ID)
	}

	record := StepRecord{
		StepID:    output.StepID,
		Status:    output.Status,
		Output:    output.Output,
		AgentID:   output.AgentID,
		Provider:  output.Provider,
		StartedAt: observed.Now,
		EndedAt:   observed.Now,
	}
	exec.RecordStep(record)
	if output.Output != "" {
		exec.SetVar("step."+output.StepID+".output", output.Output)
	}
	effects := []Effect{newRecordEffect(record)}
	if output.TerminalStatus != "" {
		effects = append(effects, Effect{
			Kind:         EffectSetTaskStatus,
			Status:       output.TerminalStatus,
			StatusReason: output.TerminalReason,
		})
		return append(effects, newWorkflowEffect(EffectCompleteWorkflow, completeExecution(exec, ExecCompleted, observed.Now))), nil
	}
	return continueFromStep(desired, observed, exec, current, effects)
}

func reduceCurrentStep(desired DesiredState, observed ObservedState, exec *Execution, current *Step, effects []Effect) ([]Effect, error) {
	visited := make(map[string]int)
	for i := range maxSyncSteps {
		if firstAt, seen := visited[current.ID]; seen {
			return nil, &CycleError{StepID: current.ID, At: i, FirstAt: firstAt}
		}
		visited[current.ID] = i

		switch current.Type {
		case StepWaitHuman:
			if exec.State == ExecWaiting && (current.Config.Status == "" || observed.Task.Status == current.Config.Status) {
				return cloneEffects(effects), nil
			}
			if current.Config.Status != "" {
				effects = append(effects, Effect{Kind: EffectSetTaskStatus, Status: current.Config.Status, StatusReason: current.Config.StatusReason})
			}
			exec.CurrentStep = current.ID
			exec.State = ExecWaiting
			effects = append(effects,
				newWorkflowEffect(EffectSetWorkflowState, exec),
				Effect{Kind: EffectWaitHuman, Step: cloneStep(current), HumanActions: slices.Clone(current.Config.HumanActions)},
			)
			return cloneEffects(effects), nil
		case StepSetStatus:
			if current.Config.Status != "" {
				effects = append(effects, Effect{Kind: EffectSetTaskStatus, Status: current.Config.Status, StatusReason: current.Config.StatusReason})
				observed.Task.Status = current.Config.Status
				observed.Task.StatusReason = current.Config.StatusReason
			}
			record := StepRecord{StepID: current.ID, Status: "completed", StartedAt: observed.Now, EndedAt: observed.Now}
			exec.RecordStep(record)
			effects = append(effects, newRecordEffect(record))
		case StepCondition:
			record := StepRecord{StepID: current.ID, Status: "completed", StartedAt: observed.Now, EndedAt: observed.Now}
			exec.RecordStep(record)
			effects = append(effects, newRecordEffect(record))
		default:
			if len(effects) > 0 || observed.Execution == nil {
				exec.CurrentStep = current.ID
				exec.State = ExecRunning
				effects = append(effects,
					newWorkflowEffect(EffectSetWorkflowState, exec),
					Effect{Kind: EffectDispatchStep, Step: cloneStep(current)},
				)
				return cloneEffects(effects), nil
			}
			return nil, nil
		}

		next, complete, err := reducerNextStep(desired, observed, exec, current)
		if err != nil {
			return nil, err
		}
		if complete {
			kind := EffectCompleteWorkflow
			if last := exec.LastRecord(); last != nil && last.Status == "failed" {
				kind = EffectFailWorkflow
			}
			effects = append(effects, newWorkflowEffect(kind, completeExecution(exec, workflowStateForKind(kind), observed.Now)))
			return cloneEffects(effects), nil
		}
		if next == nil {
			return nil, fmt.Errorf("next step missing after unresolved transition from %s", current.ID)
		}
		exec.CurrentStep = next.ID
		exec.State = ExecRunning
		current = next
	}
	return nil, fmt.Errorf("workflow exceeded max sync step depth (%d)", maxSyncSteps)
}

func continueFromStep(desired DesiredState, observed ObservedState, exec *Execution, current *Step, effects []Effect) ([]Effect, error) {
	next, complete, err := reducerNextStep(desired, observed, exec, current)
	if err != nil {
		return nil, err
	}
	if complete {
		kind := EffectCompleteWorkflow
		if observed.CompletedOutput != nil && observed.CompletedOutput.Status == "failed" {
			kind = EffectFailWorkflow
		}
		return append(cloneEffects(effects), newWorkflowEffect(kind, completeExecution(exec, workflowStateForKind(kind), observed.Now))), nil
	}
	if next == nil {
		return nil, fmt.Errorf("next step missing after unresolved transition from %s", current.ID)
	}
	exec.CurrentStep = next.ID
	exec.State = ExecRunning
	return reduceCurrentStep(desired, observed, exec, next, effects)
}

func reducerNextStep(desired DesiredState, observed ObservedState, exec *Execution, current *Step) (*Step, bool, error) {
	fields := reducerTransitionFields(desired, observed, exec)
	nextID, err := ResolveTransition(current.Next, fields)
	if err != nil {
		return nil, false, err
	}
	if nextID == "" {
		return nil, true, nil
	}
	next := desired.Definition.StepByID(nextID)
	if next == nil {
		return nil, false, fmt.Errorf("next step %s not found in workflow %s", nextID, exec.WorkflowID)
	}
	return next, false, nil
}

func previewWaitHumanByStatus(desired DesiredState, observed ObservedState, exec *Execution, current *Step) ([]Effect, bool, error) {
	status := observed.Task.Status
	if status == "" {
		return nil, false, nil
	}
	if current.Type != StepParallel && current.Type != StepBestOfN {
		return nil, false, nil
	}
	if !reducerAsyncBoundaryComplete(exec, current) {
		return nil, false, nil
	}
	fields := reducerTransitionFields(desired, observed, exec)
	nextID, err := ResolveTransition(current.Next, fields)
	if err != nil || nextID == "" {
		return nil, false, err
	}
	for range maxSyncSteps {
		step := desired.Definition.StepByID(nextID)
		if step == nil {
			return nil, false, fmt.Errorf("next step %s not found in workflow %s", nextID, exec.WorkflowID)
		}
		switch step.Type {
		case StepWaitHuman:
			if step.Config.Status != status {
				return nil, false, nil
			}
			reconciled := exec.Clone()
			if reconciled == nil {
				return nil, false, fmt.Errorf("execution clone is nil")
			}
			reconciled.CurrentStep = step.ID
			reconciled.State = ExecWaiting
			return []Effect{
				newWorkflowEffect(EffectSetWorkflowState, reconciled),
				{Kind: EffectWaitHuman, Step: cloneStep(step), HumanActions: slices.Clone(step.Config.HumanActions)},
			}, true, nil
		case StepSetStatus:
			if step.Config.Status != "" {
				fields["task.status"] = step.Config.Status
			}
		case StepCondition:
			// Keep walking.
		default:
			return nil, false, nil
		}
		nextID, err = ResolveTransition(step.Next, fields)
		if err != nil || nextID == "" {
			return nil, false, err
		}
	}
	return nil, false, fmt.Errorf("workflow exceeded max sync step depth (%d)", maxSyncSteps)
}

func reducerParksExistingExecution(step *Step) bool {
	if step == nil {
		return false
	}
	switch step.Type {
	case StepWaitHuman, StepSetStatus, StepCondition:
		return true
	default:
		return false
	}
}

func reducerTransitionFields(desired DesiredState, observed ObservedState, exec *Execution) map[string]string {
	task := observed.Task
	task.Workflow = exec
	fields := taskFields(task)
	for k, v := range exec.Variables {
		fields["vars."+k] = v
	}
	if exec.Recovered {
		fields["vars.recovered"] = "true"
	}
	fields["config.review_until_clean"] = strconv.FormatBool(desired.ReviewUntilClean)
	fields["task.review_budget_exceeded"] = strconv.FormatBool(observed.ReviewBudgetExceeded)
	maps.Copy(fields, desired.TransitionExtras)
	return fields
}

func reducerAsyncBoundaryComplete(exec *Execution, step *Step) bool {
	if exec == nil || step == nil {
		return true
	}
	switch step.Type {
	case StepParallel:
		if p := exec.ParallelInflight[step.ID]; p != nil {
			return p.AllChildrenDone()
		}
		return false
	case StepBestOfN:
		if b := exec.BestOfNInflight[step.ID]; b != nil {
			return b.AllAttemptsDone()
		}
		return false
	default:
		return true
	}
}

func reducerWorkflowID(desired DesiredState) (string, error) {
	workflowID := strings.TrimSpace(desired.WorkflowID)
	if workflowID == "" {
		workflowID = strings.TrimSpace(desired.Definition.ID)
	}
	if workflowID == "" {
		return "", fmt.Errorf("malformed reducer input: workflow id is required")
	}
	return workflowID, nil
}

func reducerStartStep(desired DesiredState) (*Step, error) {
	if startID := strings.TrimSpace(desired.StartStep); startID != "" {
		start := desired.Definition.StepByID(startID)
		if start == nil {
			return nil, fmt.Errorf("workflow %s step %s not found", desired.Definition.ID, startID)
		}
		return start, nil
	}
	start := desired.Definition.FirstStep()
	if start == nil {
		return nil, fmt.Errorf("workflow %s has no steps", desired.Definition.ID)
	}
	return start, nil
}

func completeExecution(exec *Execution, state ExecState, now time.Time) *Execution {
	completed := exec.Clone()
	if completed == nil {
		return nil
	}
	completed.CurrentStep = ""
	completed.State = state
	completedAt := now
	completed.CompletedAt = &completedAt
	return completed
}

func workflowStateForKind(kind EffectKind) ExecState {
	if kind == EffectFailWorkflow {
		return ExecFailed
	}
	return ExecCompleted
}

func newWorkflowEffect(kind EffectKind, exec *Execution) Effect {
	return Effect{Kind: kind, Workflow: exec.Clone()}
}

func newRecordEffect(record StepRecord) Effect {
	rec := record
	return Effect{Kind: EffectRecordStep, Record: &rec}
}

func cloneEffects(in []Effect) []Effect {
	out := make([]Effect, len(in))
	for i := range in {
		out[i] = Effect{
			Kind:         in[i].Kind,
			Workflow:     in[i].Workflow.Clone(),
			Status:       in[i].Status,
			StatusReason: in[i].StatusReason,
			Step:         cloneStep(in[i].Step),
			HumanActions: slices.Clone(in[i].HumanActions),
		}
		if in[i].Record != nil {
			record := *in[i].Record
			out[i].Record = &record
		}
	}
	return out
}

func cloneStep(step *Step) *Step {
	if step == nil {
		return nil
	}
	cloned := *step
	cloned.Config.AllowedTools = slices.Clone(step.Config.AllowedTools)
	cloned.Config.HumanActions = slices.Clone(step.Config.HumanActions)
	cloned.Config.ClearSidecars = slices.Clone(step.Config.ClearSidecars)
	cloned.Config.ClearWorktreeGlobs = slices.Clone(step.Config.ClearWorktreeGlobs)
	cloned.Config.AttemptProviders = slices.Clone(step.Config.AttemptProviders)
	cloned.Parallel = cloneSteps(step.Parallel)
	if step.Config.Check != nil {
		check := *step.Config.Check
		cloned.Config.Check = &check
	}
	if step.Position != nil {
		pos := *step.Position
		cloned.Position = &pos
	}
	if step.Next != nil {
		cloned.Next = make([]Transition, len(step.Next))
		for i := range step.Next {
			cloned.Next[i] = step.Next[i]
			if step.Next[i].When != nil {
				when := *step.Next[i].When
				cloned.Next[i].When = &when
			}
		}
	}
	return &cloned
}

func cloneSteps(steps []Step) []Step {
	if steps == nil {
		return nil
	}
	out := make([]Step, len(steps))
	for i := range steps {
		out[i] = *cloneStep(&steps[i])
	}
	return out
}
