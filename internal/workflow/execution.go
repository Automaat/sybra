package workflow

import (
	"maps"
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
	// StepCounts tracks, per step ID, how many times RecordStep has recorded
	// that step — independent of StepHistory, which RecordStep trims to
	// maxStepHistory. Replan/retry budgets (CountStep) read from this map so
	// history eviction can never silently reset a cap. ClearStepRecords resets
	// a step's count deliberately, for the re-arm case (a fresh attempt cycle
	// that should not inherit a prior cycle's budget).
	StepCounts map[string]int `yaml:"step_counts,omitempty" json:"stepCounts,omitempty"`
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

// Clone returns a deep copy of the execution so callers can persist or restore
// workflow state without aliasing mutable maps/slices from the original.
func (e *Execution) Clone() *Execution {
	if e == nil {
		return nil
	}
	cloned := *e
	if e.StepHistory != nil {
		cloned.StepHistory = slices.Clone(e.StepHistory)
	}
	if e.Variables != nil {
		cloned.Variables = maps.Clone(e.Variables)
	}
	if e.StepCounts != nil {
		cloned.StepCounts = maps.Clone(e.StepCounts)
	}
	if e.ParallelInflight != nil {
		cloned.ParallelInflight = make(map[string]*ParallelChildren, len(e.ParallelInflight))
		for key, child := range e.ParallelInflight {
			if child == nil {
				cloned.ParallelInflight[key] = nil
				continue
			}
			childClone := *child
			if child.Children != nil {
				childClone.Children = make(map[string]*ChildStatus, len(child.Children))
				for childID, status := range child.Children {
					if status == nil {
						childClone.Children[childID] = nil
						continue
					}
					statusClone := *status
					childClone.Children[childID] = &statusClone
				}
			}
			cloned.ParallelInflight[key] = &childClone
		}
	}
	return &cloned
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

// RecordStep appends a step record, trims history to maxStepHistory, and
// increments the step's persistent count (see StepCounts) so trimming never
// affects CountStep. On the first call for an Execution whose StepCounts is
// still nil (a task loaded from disk before this field existed, or a
// hand-built fixture), the map is seeded by counting whatever StepHistory
// already holds — a one-time migration of the still-live (unevicted) window
// — before the new record is added.
func (e *Execution) RecordStep(r StepRecord) {
	if e.StepCounts == nil {
		e.StepCounts = stepCountsFromHistory(e.StepHistory)
	}
	e.StepHistory = append(e.StepHistory, r)
	if len(e.StepHistory) > maxStepHistory {
		e.StepHistory = e.StepHistory[len(e.StepHistory)-maxStepHistory:]
	}
	e.StepCounts[r.StepID]++
}

func stepCountsFromHistory(history []StepRecord) map[string]int {
	counts := make(map[string]int, len(history))
	for i := range history {
		counts[history[i].StepID]++
	}
	return counts
}

// LastRecord returns the most recent step record, or nil.
func (e *Execution) LastRecord() *StepRecord {
	if len(e.StepHistory) == 0 {
		return nil
	}
	return &e.StepHistory[len(e.StepHistory)-1]
}

// CountStep returns the number of times a given step ID has been recorded.
// Prefers the persistent StepCounts map (unaffected by StepHistory trimming);
// falls back to scanning StepHistory when StepCounts has never been
// initialized (RecordStep seeds it lazily — see there) or holds no entry for
// this step, e.g. right after ClearStepRecords.
func (e *Execution) CountStep(stepID string) int {
	if e.StepCounts != nil {
		if n, ok := e.StepCounts[stepID]; ok {
			return n
		}
	}
	n := 0
	for i := range e.StepHistory {
		if e.StepHistory[i].StepID == stepID {
			n++
		}
	}
	return n
}

// ClearStepRecords removes all history records for a given step ID and resets
// its persistent count (see StepCounts), resetting CountStep(stepID) to 0.
// Used when a step is deliberately re-armed for a fresh attempt cycle (e.g.
// route-level auto-retry) so its own in-step max_retries budget is not seen
// as already exhausted by prior cycles.
func (e *Execution) ClearStepRecords(stepID string) {
	if len(e.StepHistory) > 0 {
		kept := e.StepHistory[:0]
		for i := range e.StepHistory {
			if e.StepHistory[i].StepID != stepID {
				kept = append(kept, e.StepHistory[i])
			}
		}
		e.StepHistory = kept
	}
	delete(e.StepCounts, stepID)
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

// LastAgentStepFailed reports whether the most recent step that ran an agent
// (AgentID set) terminated with status "failed". verify_commits uses this to
// tell "agent crashed before committing" (→ human-required) apart from "branch
// already merged into base" (→ done): both leave a fresh worktree branch on the
// base tip with no commits ahead, so git alone cannot distinguish them.
func (e *Execution) LastAgentStepFailed() bool {
	for i := range slices.Backward(e.StepHistory) {
		if e.StepHistory[i].AgentID != "" {
			return e.StepHistory[i].Status == "failed"
		}
	}
	return false
}

// LastAgentID returns the agent ID of the most recent step that ran an agent,
// or "" if none. verify_commits uses it to identify the just-completed agent
// (whose own completion triggered the step) so it can be excluded when checking
// for a sibling agent still working the task.
func (e *Execution) LastAgentID() string {
	for i := range slices.Backward(e.StepHistory) {
		if e.StepHistory[i].AgentID != "" {
			return e.StepHistory[i].AgentID
		}
	}
	return ""
}

// LastAgentStepID returns the step ID of the most recent step that ran an agent
// (always a run_agent step, the only kind that records an AgentID), or "" if
// none. verify_commits uses it to re-arm that step when parking the workflow to
// wait out a sibling agent, so ResumeStalled — which only resumes run_agent
// steps — re-drives it.
func (e *Execution) LastAgentStepID() string {
	for i := range slices.Backward(e.StepHistory) {
		if e.StepHistory[i].AgentID != "" {
			return e.StepHistory[i].StepID
		}
	}
	return ""
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
