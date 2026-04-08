package workflow

import "time"

// ExecState tracks the overall execution state.
type ExecState string

const (
	ExecRunning   ExecState = "running"
	ExecWaiting   ExecState = "waiting"
	ExecCompleted ExecState = "completed"
	ExecFailed    ExecState = "failed"
)

// Execution tracks a task's progress through a workflow instance.
type Execution struct {
	WorkflowID  string            `yaml:"workflow_id" json:"workflowId"`
	CurrentStep string            `yaml:"current_step" json:"currentStep"`
	State       ExecState         `yaml:"state" json:"state"`
	StepHistory []StepRecord      `yaml:"step_history,omitempty" json:"stepHistory"`
	Variables   map[string]string `yaml:"variables,omitempty" json:"variables"`
	StartedAt   time.Time         `yaml:"started_at" json:"startedAt"`
	CompletedAt *time.Time        `yaml:"completed_at,omitempty" json:"completedAt"`
}

// SetVar sets a variable in the execution context.
func (e *Execution) SetVar(key, value string) {
	if e.Variables == nil {
		e.Variables = make(map[string]string)
	}
	e.Variables[key] = value
}

// RecordStep appends a step record and trims history to maxStepHistory.
func (e *Execution) RecordStep(r StepRecord) {
	e.StepHistory = append(e.StepHistory, r)
	if len(e.StepHistory) > maxStepHistory {
		e.StepHistory = e.StepHistory[len(e.StepHistory)-maxStepHistory:]
	}
}

// LastRecord returns the most recent step record, or nil.
func (e *Execution) LastRecord() *StepRecord {
	if len(e.StepHistory) == 0 {
		return nil
	}
	return &e.StepHistory[len(e.StepHistory)-1]
}

// CountStep returns the number of records for a given step ID.
func (e *Execution) CountStep(stepID string) int {
	n := 0
	for i := range e.StepHistory {
		if e.StepHistory[i].StepID == stepID {
			n++
		}
	}
	return n
}

// RecordForStep returns the latest record for a given step ID, or nil.
func (e *Execution) RecordForStep(stepID string) *StepRecord {
	for i := len(e.StepHistory) - 1; i >= 0; i-- {
		if e.StepHistory[i].StepID == stepID {
			return &e.StepHistory[i]
		}
	}
	return nil
}

// StepRecord captures the result of executing one step.
type StepRecord struct {
	StepID    string    `yaml:"step_id" json:"stepId"`
	Status    string    `yaml:"status" json:"status"` // "completed", "failed", "skipped"
	Output    string    `yaml:"output,omitempty" json:"output"`
	AgentID   string    `yaml:"agent_id,omitempty" json:"agentId"`
	StartedAt time.Time `yaml:"started_at" json:"startedAt"`
	EndedAt   time.Time `yaml:"ended_at,omitempty" json:"endedAt"`
}

// StepOutput is passed to AdvanceStep when a step finishes.
type StepOutput struct {
	StepID  string
	Status  string // "completed", "failed"
	Output  string
	AgentID string
}
