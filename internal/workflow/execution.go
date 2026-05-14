package workflow

import (
	"slices"
	"time"
)

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
	// Recovered is set when the execution was advanced by a stale-session
	// recovery path rather than a live agent result. Cleared after the first
	// step consumes it. Workflow prompts should use recoveredOrPrev instead of
	// .Prev.Output to guard against stale content.
	Recovered bool `yaml:"recovered,omitempty" json:"recovered,omitempty"`
	// ParallelInflight tracks per-parent-step state for in-flight `parallel`
	// blocks. Keyed by the parent (parallel) step ID. The engine populates
	// it on dispatch and clears it once every child has terminated.
	// Persisted so a process restart can resume the parent step rather than
	// stranding half-completed forks.
	ParallelInflight map[string]*ParallelChildren `yaml:"parallel_inflight,omitempty" json:"parallelInflight,omitempty"`
}

// ParallelChildren is the in-flight bookkeeping for one `parallel` parent.
type ParallelChildren struct {
	ParentStepID string                  `yaml:"parent_step_id" json:"parentStepId"`
	StartedAt    time.Time               `yaml:"started_at" json:"startedAt"`
	Children     map[string]*ChildStatus `yaml:"children" json:"children"`
}

// ChildStatus is one child step's slot inside a ParallelChildren record.
type ChildStatus struct {
	AgentID  string `yaml:"agent_id,omitempty" json:"agentId,omitempty"`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// Status: "pending" | "completed" | "failed".
	Status  string `yaml:"status" json:"status"`
	Output  string `yaml:"output,omitempty" json:"output,omitempty"`
	Retries int    `yaml:"retries,omitempty" json:"retries,omitempty"`
}

// AllChildrenDone reports whether every child reached a terminal status.
func (p *ParallelChildren) AllChildrenDone() bool {
	if p == nil {
		return false
	}
	for _, c := range p.Children {
		if c == nil {
			return false
		}
		if c.Status != "completed" && c.Status != "failed" {
			return false
		}
	}
	return len(p.Children) > 0
}

// AnyChildFailed reports whether any child terminated with status=failed.
// Caller decides whether to mark the parent failed or proceed.
func (p *ParallelChildren) AnyChildFailed() bool {
	if p == nil {
		return false
	}
	for _, c := range p.Children {
		if c == nil {
			continue
		}
		if c.Status == "failed" {
			return true
		}
	}
	return false
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
	for i := range slices.Backward(e.StepHistory) {
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
	Provider  string    `yaml:"provider,omitempty" json:"provider,omitempty"`
	StartedAt time.Time `yaml:"started_at" json:"startedAt"`
	EndedAt   time.Time `yaml:"ended_at,omitempty" json:"endedAt"`
}

// StepOutput is passed to AdvanceStep when a step finishes.
type StepOutput struct {
	StepID   string
	Status   string // "completed", "failed"
	Output   string
	AgentID  string
	Provider string
}
