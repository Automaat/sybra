// Package intervention captures a genuine operator (or automatic-recovery)
// unblock of a human-required task as a normalized, fingerprint-deduplicated
// record: what kind of blocker parked the task, what the system already
// tried, who resolved it, and where it went. Records are advisory — they feed
// no deterministic routing/admission/completion gate — and exist so repeats
// of the same autonomy failure aggregate into one scenario instead of
// consuming operator attention over and over (sybra#2468).
//
// Stores are scrub-agnostic, same discipline as internal/experience: callers
// must scrub work-derived fields before Put.
package intervention

import "time"

// ReplayStatusUnsupportedSimulation marks a record that has not been wired
// into #2454's deterministic replay harness yet — the harness only consumes
// scenario-string fixtures today, not a generic fingerprint registry. Every
// record captured by this increment gets this value so a novel fingerprint is
// still durably registered, ready for #2454 to pick up later.
const ReplayStatusUnsupportedSimulation = "unsupported_simulation"

// OperatorActionClass classifies who resolved the human-required park.
type OperatorActionClass string

const (
	// OperatorActionHuman means a person clicked Dispatch from the
	// human-required panel.
	OperatorActionHuman OperatorActionClass = "human"
	// OperatorActionAutoRecovery means an in-process recovery path
	// re-entered the workflow on the operator's behalf (e.g. human-review's
	// dispatchFromHumanRequired with a completing agent ID).
	OperatorActionAutoRecovery OperatorActionClass = "auto_recovery"
)

// Record is a compact, typed capture of one genuine unblock of a
// human-required task: the blocker that parked it, the system-state
// transition, what was already attempted, and who resolved it. Never carries
// raw work content — work-typed tasks route through scrub before Put.
type Record struct {
	TaskID      string    `json:"task_id"`
	CreatedAt   time.Time `json:"created_at"`
	ProjectID   string    `json:"project_id"`
	ProjectType string    `json:"project_type"`
	// BlockerKind/BlockerCode mirror blocker.State's typed classification —
	// stable, short, code-authored tokens (e.g. "operator_decision",
	// "disk_space"), never free-form agent/user text.
	BlockerKind string `json:"blocker_kind,omitempty"`
	BlockerCode string `json:"blocker_code,omitempty"`
	FromStatus  string `json:"from_status"`
	ToStatus    string `json:"to_status"`
	// WorkflowStep is the workflow engine step ID the task was stuck at, if
	// any (task.Workflow.CurrentStep).
	WorkflowStep string `json:"workflow_step,omitempty"`
	// AttemptedActions lists what the system already tried before parking:
	// the roles of prior agent runs against this task plus the terminal
	// status_reason, deduplicated and order-preserving.
	AttemptedActions    []string            `json:"attempted_actions,omitempty"`
	OperatorActionClass OperatorActionClass `json:"operator_action_class"`
	OperatorReason      string              `json:"operator_reason,omitempty"`
	Fingerprint         string              `json:"fingerprint"`
	// Recurrences counts how many times an equivalent intervention (same
	// Fingerprint) has been observed, including this one. Store.Put
	// maintains this — never set it directly outside the package.
	Recurrences int       `json:"recurrences"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	// ReplayStatus records whether this fingerprint has been converted into a
	// deterministic replay fixture for #2454's harness. Defaults to
	// ReplayStatusUnsupportedSimulation until that wiring exists.
	ReplayStatus string `json:"replay_status"`
}
