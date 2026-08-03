package workflow

import (
	"fmt"
	"strings"
)

// EffectID identifies one reducer-emitted effect within a task generation.
// Generation scopes ids across retries/resets, StepSeq orders steps within that
// generation, StepID anchors the originating workflow step, and Pos
// distinguishes sibling effects from the same reducer decision.
type EffectID struct {
	Generation int64  `yaml:"generation" json:"generation"`
	StepSeq    int    `yaml:"step_seq" json:"stepSeq"`
	StepID     string `yaml:"step_id" json:"stepID"`
	Pos        int    `yaml:"pos" json:"pos"`
}

// IsZero reports whether the id has not been assigned yet.
func (id EffectID) IsZero() bool {
	return id.Generation == 0 && id.StepSeq == 0 && id.StepID == "" && id.Pos == 0
}

// Equal reports whether two ids name the same logical effect.
func (id EffectID) Equal(other EffectID) bool {
	return id == other
}

// Less reports whether id sorts before other in monotonic execution order.
func (id EffectID) Less(other EffectID) bool {
	return id.Compare(other) < 0
}

// Before is an alias for Less.
func (id EffectID) Before(other EffectID) bool {
	return id.Less(other)
}

// Compare returns -1, 0, or 1 using the reducer's monotonic ordering.
func (id EffectID) Compare(other EffectID) int {
	switch {
	case id.Generation < other.Generation:
		return -1
	case id.Generation > other.Generation:
		return 1
	case id.StepSeq < other.StepSeq:
		return -1
	case id.StepSeq > other.StepSeq:
		return 1
	}
	if cmp := strings.Compare(id.StepID, other.StepID); cmp != 0 {
		return cmp
	}
	switch {
	case id.Pos < other.Pos:
		return -1
	case id.Pos > other.Pos:
		return 1
	}
	return 0
}

func (id EffectID) String() string {
	if id.IsZero() {
		return "effect:zero"
	}
	return fmt.Sprintf("g%d:s%d:%s:%d", id.Generation, id.StepSeq, id.StepID, id.Pos)
}

func assignEffectIDs(desired DesiredState, observed ObservedState, effects []Effect) []Effect {
	if len(effects) == 0 {
		return effects
	}
	currentStepID, currentSeq := initialEffectCursor(desired, observed)
	pos := 0
	for i := range effects {
		stepID := effectAnchorStepID(effects, i, currentStepID)
		switch {
		case stepID == "":
			stepID = currentStepID
		case currentStepID == "":
			currentStepID = stepID
		case stepID != currentStepID:
			currentSeq++
			currentStepID = stepID
			pos = 0
		}
		effects[i].ID = EffectID{
			Generation: observed.Task.Generation,
			StepSeq:    currentSeq,
			StepID:     stepID,
			Pos:        pos,
		}
		pos++
	}
	return effects
}

func initialEffectCursor(desired DesiredState, observed ObservedState) (stepID string, stepSeq int) {
	stepSeq = executionStepSeq(observed.Execution)
	if observed.Execution == nil {
		start, err := reducerStartStep(desired)
		if err != nil || start == nil {
			return "", stepSeq
		}
		return start.ID, stepSeq
	}
	stepID = observed.Execution.CurrentStep
	if stepID == "" {
		return "", stepSeq
	}
	current := desired.Definition.StepByID(stepID)
	if current == nil {
		return stepID, stepSeq
	}
	if observed.CompletedOutput == nil && !workflowBoundaryOpen(current, observed.Execution) {
		return "", stepSeq
	}
	return stepID, stepSeq
}

func effectAnchorStepID(effects []Effect, idx int, fallback string) string {
	effect := effects[idx]
	switch effect.Kind {
	case EffectRecordStep:
		if effect.Record != nil {
			return effect.Record.StepID
		}
	case EffectDispatchStep, EffectWaitHuman:
		if effect.Step != nil {
			return effect.Step.ID
		}
	case EffectSetWorkflowState:
		if effect.Workflow != nil && effect.Workflow.CurrentStep != "" {
			return effect.Workflow.CurrentStep
		}
	case EffectSetTaskStatus:
		if idx+1 < len(effects) {
			switch next := effects[idx+1]; next.Kind {
			case EffectRecordStep:
				if next.Record != nil {
					return next.Record.StepID
				}
			case EffectSetWorkflowState:
				if next.Workflow != nil && next.Workflow.CurrentStep != "" {
					return next.Workflow.CurrentStep
				}
			case EffectDispatchStep, EffectWaitHuman:
				if next.Step != nil {
					return next.Step.ID
				}
			case EffectSetTaskStatus, EffectCompleteWorkflow, EffectFailWorkflow:
			}
		}
	case EffectCompleteWorkflow, EffectFailWorkflow:
	}
	return fallback
}

func executionStepSeq(exec *Execution) int {
	if exec == nil {
		return 0
	}
	if len(exec.StepCounts) == 0 {
		return len(exec.StepHistory)
	}
	total := 0
	for _, count := range exec.StepCounts {
		total += count
	}
	return total
}

func workflowBoundaryOpen(step *Step, exec *Execution) bool {
	if step == nil || exec == nil {
		return true
	}
	boundary, ok := stepBoundary(step.Type)
	if !ok {
		return true
	}
	switch boundary {
	case stepBoundaryParallel, stepBoundaryBestOfN:
		return !reducerAsyncBoundaryComplete(exec, step)
	default:
		return true
	}
}
