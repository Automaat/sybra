package task

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/workflow"
)

type Status string

const (
	StatusNew           Status = "new"
	StatusTodo          Status = "todo"
	StatusInProgress    Status = "in-progress"
	StatusReadyReview   Status = "ready-review"
	StatusInReview      Status = "in-review"
	StatusPlanning      Status = "planning"
	StatusPlanReview    Status = "plan-review"
	StatusTesting       Status = "testing"
	StatusReadyPR       Status = "ready-pr"
	StatusHumanRequired Status = "human-required"
	StatusBlocked       Status = "blocked"
	StatusDone          Status = "done"
	StatusCancelled     Status = "cancelled"
)

var validStatuses = map[Status]bool{
	StatusNew: true, StatusTodo: true, StatusInProgress: true,
	StatusReadyReview: true, StatusInReview: true,
	StatusPlanning: true, StatusPlanReview: true,
	StatusTesting: true, StatusReadyPR: true,
	StatusHumanRequired: true, StatusBlocked: true,
	StatusDone: true, StatusCancelled: true,
}

// AllStatuses returns every valid status in display order.
func AllStatuses() []Status {
	return []Status{
		StatusNew, StatusTodo, StatusPlanning, StatusPlanReview,
		StatusInProgress, StatusReadyReview, StatusInReview,
		StatusTesting, StatusReadyPR,
		StatusHumanRequired, StatusBlocked, StatusDone, StatusCancelled,
	}
}

// IsTerminalStatus reports whether s is a terminal (closed) status.
func IsTerminalStatus(s Status) bool {
	return s == StatusDone || s == StatusCancelled
}

func ValidateStatus(s string) (Status, error) {
	st := Status(s)
	if !validStatuses[st] {
		return "", fmt.Errorf("invalid status %q (valid: %v)", s, AllStatuses())
	}
	return st, nil
}

type Priority string

const (
	PriorityNone   Priority = ""
	PriorityLow    Priority = "low"
	PriorityMedium Priority = "medium"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

var validPriorities = map[Priority]bool{
	PriorityNone: true, PriorityLow: true, PriorityMedium: true,
	PriorityHigh: true, PriorityUrgent: true,
}

func ValidatePriority(s string) (Priority, error) {
	p := Priority(s)
	if !validPriorities[p] {
		return "", fmt.Errorf("invalid priority %q (valid: none, low, medium, high, urgent)", s)
	}
	return p, nil
}

type TaskType string

const (
	TaskTypeNormal   TaskType = "normal"
	TaskTypeDebug    TaskType = "debug"
	TaskTypeResearch TaskType = "research"
	// TaskTypeChat is a synthetic task created for interactive chat sessions.
	// Hidden from the task list UI and skipped by restart-stale/watchdog.
	TaskTypeChat TaskType = "chat"
	// TaskTypeUmbrella is the tracker task for an expanded ☂️ umbrella issue.
	// It runs no agent: it rolls up the status of its child tasks and is the
	// task the dependency gate flips to human-required on a dependency cycle.
	TaskTypeUmbrella TaskType = "umbrella"
)

var validTaskTypes = map[TaskType]bool{
	TaskTypeNormal: true, TaskTypeDebug: true, TaskTypeResearch: true,
	TaskTypeChat: true, TaskTypeUmbrella: true,
}

// AllTaskTypes returns every valid task type in display order.
func AllTaskTypes() []TaskType {
	return []TaskType{TaskTypeNormal, TaskTypeDebug, TaskTypeResearch, TaskTypeChat, TaskTypeUmbrella}
}

func ValidateTaskType(s string) (TaskType, error) {
	tt := TaskType(s)
	if !validTaskTypes[tt] {
		return "", fmt.Errorf("invalid task_type %q (valid: %v)", s, AllTaskTypes())
	}
	return tt, nil
}

const (
	AgentModeHeadless    = "headless"
	AgentModeInteractive = "interactive"
)

var validAgentModes = map[string]bool{
	AgentModeHeadless: true, AgentModeInteractive: true,
}

// AllAgentModes returns every valid agent mode in display order.
func AllAgentModes() []string {
	return []string{AgentModeHeadless, AgentModeInteractive}
}

// ValidateAgentMode rejects unknown agent modes. Empty strings are rejected
// here; callers that need to allow "unset" (e.g. parser legacy compat) must
// guard the empty case before calling.
func ValidateAgentMode(s string) (string, error) {
	if !validAgentModes[s] {
		return "", fmt.Errorf("invalid agent_mode %q (valid: %v)", s, AllAgentModes())
	}
	return s, nil
}

// AllReasoningEfforts returns every valid codex reasoning effort level.
func AllReasoningEfforts() []string { return []string{"low", "medium", "high", "xhigh"} }

var validReasoningEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true}

// ValidateReasoningEffort accepts the empty string (model default) or one of the
// codex-advertised levels. Static allowlist — offline-safe, no codex probe.
func ValidateReasoningEffort(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !validReasoningEfforts[s] {
		return "", fmt.Errorf("invalid reasoning_effort %q (valid: low, medium, high, xhigh, or empty)", s)
	}
	return s, nil
}

// AllAgentProviders returns every supported CLI provider name in display order.
func AllAgentProviders() []string { return []string{"claude", "codex", "copilot"} }

var validAgentProviders = map[string]bool{"claude": true, "codex": true, "copilot": true}

// ValidateAgentProvider accepts the empty string (unset/default) or one of the
// providers Sybra can dispatch. It is used for handoff provenance, not the
// task's execution mode.
func ValidateAgentProvider(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !validAgentProviders[s] {
		return "", fmt.Errorf("invalid provider %q (valid: claude, codex, copilot, or empty)", s)
	}
	return s, nil
}

type AgentRun struct {
	AgentID  string `yaml:"agent_id" json:"agentId"`
	Role     string `yaml:"role,omitempty" json:"role"` // triage, plan, eval, pr-fix, or "" for implementation
	Mode     string `yaml:"mode" json:"mode"`
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model    string `yaml:"model,omitempty" json:"model,omitempty"`
	// ExperimentID/VariantID capture deterministic A/B assignment selected
	// before the run started.
	ExperimentID    string    `yaml:"experiment_id,omitempty" json:"experimentId,omitempty"`
	VariantID       string    `yaml:"variant_id,omitempty" json:"variantId,omitempty"`
	AssignmentUnit  string    `yaml:"assignment_unit,omitempty" json:"assignmentUnit,omitempty"`
	AssignmentKey   string    `yaml:"assignment_key,omitempty" json:"assignmentKey,omitempty"`
	ReasoningEffort string    `yaml:"reasoning_effort,omitempty" json:"reasoningEffort,omitempty"`
	State           string    `yaml:"state" json:"state"`
	StartedAt       time.Time `yaml:"started_at" json:"startedAt"`
	CostUSD         float64   `yaml:"cost_usd,omitempty" json:"costUsd"`
	PremiumRequests float64   `yaml:"premium_requests,omitempty" json:"premiumRequests,omitempty"`
	Prompt          string    `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Result          string    `yaml:"result,omitempty" json:"result"`
	// Verdict holds the parsed decision for human-review runs ("human" or
	// "sybra_bug"). Extracted from live agent output at completion time so
	// it survives Result truncation.
	Verdict string `yaml:"verdict,omitempty" json:"verdict,omitempty"`
	// VerdictRendered is set to true after onComplete has successfully applied
	// all side-effects for this run (note appended, issue filed, local task
	// created). Used by verdictAlreadyRendered as the durable rendered-marker
	// instead of body-text patterns which can collide with user content.
	VerdictRendered bool   `yaml:"verdict_rendered,omitempty" json:"verdictRendered,omitempty"`
	LogFile         string `yaml:"log_file,omitempty" json:"logFile"`
	SessionID       string `yaml:"session_id,omitempty" json:"sessionId,omitempty"`
	// ProtocolViolation records deterministic workflow-level contract failures
	// for this run. Test routing uses it to avoid counting a bad verifier report
	// as an implementation failure across later workflow executions.
	ProtocolViolation string `yaml:"protocol_violation,omitempty" json:"protocolViolation,omitempty"`
	// TestOutcome records the deterministic classification of a test-runner run
	// (product_bug, infra_failure, ambiguous_requirement, etc.). The testing
	// router counts only product_bug outcomes against the implementation budget.
	TestOutcome string `yaml:"test_outcome,omitempty" json:"testOutcome,omitempty"`
	// TestFailureFingerprint is a stable hash of the grounded failure report.
	// Repeated fingerprints mean the same repro survived a fix attempt and should
	// get targeted human/local reproduction instead of another broad retry.
	TestFailureFingerprint string `yaml:"test_failure_fingerprint,omitempty" json:"testFailureFingerprint,omitempty"`
	// HeadSHA is the worktree HEAD commit at this run's completion — what the
	// agent left on the branch. Compared against the merged PR head to detect
	// human edits after the agent (merged_with_edits) and measure edit distance.
	HeadSHA string `yaml:"head_sha,omitempty" json:"headSha,omitempty"`
}

type Task struct {
	ID           string   `yaml:"id" json:"id"`
	Slug         string   `yaml:"slug,omitempty" json:"slug"`
	Title        string   `yaml:"title" json:"title"`
	Status       Status   `yaml:"status" json:"status"`
	TaskType     TaskType `yaml:"task_type,omitempty" json:"taskType"`
	AgentMode    string   `yaml:"agent_mode" json:"agentMode"`
	AllowedTools []string `yaml:"allowed_tools" json:"allowedTools"`
	Tags         []string `yaml:"tags" json:"tags"`
	ProjectID    string   `yaml:"project_id,omitempty" json:"projectId"`
	Branch       string   `yaml:"branch,omitempty" json:"branch"`
	// WorktreeDir, when non-empty, makes Sybra adopt an externally-created git
	// worktree at this path instead of creating its own under
	// ~/.sybra/worktrees. Used for handoff from tools like Orca: every agent
	// for this task runs in this directory, and Sybra never rebases, force-
	// pushes, or removes it — the external tool owns its lifecycle. Empty =
	// Sybra-managed worktree (default).
	WorktreeDir  string `yaml:"worktree_dir,omitempty" json:"worktreeDir,omitempty"`
	PRNumber     int    `yaml:"pr_number,omitempty" json:"prNumber"`
	Issue        string `yaml:"issue,omitempty" json:"issue"`
	StatusReason string `yaml:"status_reason,omitempty" json:"statusReason"`
	// HandoffSourceProvider records which local agent provider produced the
	// work before a handoff skipped directly into review/testing/PR. Workflow
	// steps with provider=cross use it when there is no Sybra-authored run
	// history to flip from.
	HandoffSourceProvider string `yaml:"handoff_source_provider,omitempty" json:"handoffSourceProvider,omitempty"`
	// BlockedByIssue stores the URL of the GitHub issue that put the task
	// into status=blocked. Set by the human-review automation when it
	// concludes the human-required transition was caused by a Sybra bug.
	BlockedByIssue string `yaml:"blocked_by_issue,omitempty" json:"blockedByIssue,omitempty"`
	// UmbrellaIssue links this task to the ☂️ umbrella issue it was expanded
	// from. Set on child tasks; empty for standalone tasks. The orchestrator's
	// dependency gate reads it with DependsOn to decide when a child may leave
	// `blocked`.
	UmbrellaIssue string `yaml:"umbrella_issue,omitempty" json:"umbrellaIssue,omitempty"`
	// DependsOn lists the issue refs (full github.com issue/PR URL or
	// owner/repo#n shorthand) this task waits on — resolved by issue ref only,
	// not task IDs. While the task is `blocked`, the gate holds it until every
	// referenced task has reached `done`; an empty list releases immediately.
	// Used only by umbrella child tasks.
	DependsOn []string `yaml:"depends_on,omitempty" json:"dependsOn,omitempty"`
	Reviewed  bool     `yaml:"reviewed,omitempty" json:"reviewed"`
	RunRole   string   `yaml:"run_role,omitempty" json:"runRole"` // pr-fix when fixing review issues, "" for initial impl
	// SupervisorSteer is a one-shot corrective message left by the watchdog's
	// headless nudge: it stops a looping headless agent (which has no mid-stream
	// channel) and persists the steer here so the recovery loop re-dispatches
	// the resumed agent with the correction prepended to its prompt. Recovery
	// clears it after consuming it exactly once. Empty for tasks with no pending
	// nudge.
	SupervisorSteer string `yaml:"supervisor_steer,omitempty" json:"supervisorSteer,omitempty"`
	// ReviewPhase tracks where an inbound PR-review task (tag `review`) sits in
	// the review lifecycle: reviewing → drafted → awaiting-author →
	// needs-approval → approved (plus `manual` for small PRs punted to the
	// human). Computed by the PR poller; drives the board's PR Reviews lane.
	// Empty for non-review tasks.
	ReviewPhase string `yaml:"review_phase,omitempty" json:"reviewPhase,omitempty"`
	// PRPhase tracks where an outbound own-PR task (status in-review/ready-review,
	// not tag `review`) sits in its lifecycle: draft → building → fixing →
	// changes-requested → awaiting-approval → approved. Computed by the PR poller;
	// drives the phase glyph on the board's In Review cards. Empty for tasks
	// without an own PR (and for inbound review tasks, which use ReviewPhase).
	PRPhase   string     `yaml:"pr_phase,omitempty" json:"prPhase,omitempty"`
	TodoistID string     `yaml:"todoist_id,omitempty" json:"todoistId"`
	Priority  Priority   `yaml:"priority,omitempty" json:"priority,omitempty"`
	DueDate   *time.Time `yaml:"due_date,omitempty" json:"dueDate,omitempty"`
	ClosedAt  *time.Time `yaml:"closed_at,omitempty" json:"closedAt,omitempty"`
	// Outcome records how a task's own PR concluded: "merged", "merged_with_edits",
	// "closed", or "reverted". Stamped by the PR monitor when the task auto-advances
	// to done (and updated to "reverted" by the revert scanner). Empty for tasks
	// that never produced a PR. Feeds the evaluation scorecard.
	Outcome string `yaml:"outcome,omitempty" json:"outcome,omitempty"`
	// MergeCommit is the default-branch commit a merged PR produced, recorded at
	// landing so the revert scanner can detect a later revert of it.
	MergeCommit string `yaml:"merge_commit,omitempty" json:"mergeCommit,omitempty"`
	// MaxTurns overrides the global agent turn limit for this task.
	// Zero means "use global default".
	MaxTurns int `yaml:"max_turns,omitempty" json:"maxTurns,omitempty"`
	// RequirePermissions overrides the system default when set.
	// nil = use system default (true). false = opt out (--dangerously-skip-permissions).
	RequirePermissions *bool `yaml:"require_permissions,omitempty" json:"requirePermissions,omitempty"`
	// HeadlessPermissionMode overrides the permission posture for headless claude
	// runs on this task. "auto" emits --permission-mode auto; "bypass" keeps
	// --dangerously-skip-permissions. Empty = use config default.
	HeadlessPermissionMode string `yaml:"headless_permission_mode,omitempty" json:"headlessPermissionMode,omitempty"`
	// ForkSubagent enables CLAUDE_CODE_FORK_SUBAGENT=1 for this task's headless
	// agent, allowing a single prompt to spawn parallel subagent runs. Trades
	// higher token cost for reduced wall-clock time on multi-part prompts.
	ForkSubagent bool `yaml:"fork_subagent,omitempty" json:"forkSubagent,omitempty"`
	// ReasoningEffort sets the Codex model reasoning level for this task's agents
	// via -c model_reasoning_effort=<v>. Empty = model default. Codex-only;
	// ignored for claude agents. Distinct from the claude-only extended-thinking
	// knob — different CLI surface and vocabulary (xhigh vs max).
	ReasoningEffort string `yaml:"reasoning_effort,omitempty" json:"reasoningEffort,omitempty"`
	// TestingCycleStartedAt marks the start of the current testing cycle. It is
	// set when a human re-dispatches the task after a human-required escalation,
	// so route_test_result can exclude test-runner runs from prior cycles when
	// counting toward TestingMaxAttempts. Nil means no re-dispatch has occurred
	// and all test-runner runs count (correct for first-ever cycles).
	TestingCycleStartedAt *time.Time          `yaml:"testing_cycle_started_at,omitempty" json:"testingCycleStartedAt,omitempty"`
	AgentRuns             []AgentRun          `yaml:"agent_runs,omitempty" json:"agentRuns"`
	Workflow              *workflow.Execution `yaml:"workflow,omitempty" json:"workflow"`
	CreatedAt             time.Time           `yaml:"created_at" json:"createdAt"`
	UpdatedAt             time.Time           `yaml:"updated_at" json:"updatedAt"`

	Body         string `yaml:"-" json:"body"`
	Plan         string `yaml:"-" json:"plan,omitempty"`
	PlanContract string `yaml:"-" json:"planContract,omitempty"`
	PlanCritique string `yaml:"-" json:"planCritique,omitempty"`
	// Planning sidecars. Plan is the human-readable compact plan; PlanContract
	// is the machine-validated JSON contract consumed by implementation agents.
	// The remaining sidecars hold review/evidence material.
	PlanResearch  string `yaml:"-" json:"planResearch,omitempty"`
	PlanDecisions string `yaml:"-" json:"planDecisions,omitempty"`
	PlanBrief     string `yaml:"-" json:"planBrief,omitempty"`
	CodeReview    string `yaml:"-" json:"codeReview,omitempty"`
	// PlanDrafts holds per-provider raw plan outputs during dual- (or N-)
	// provider planning. Keys are typically the parallel child step ID
	// (e.g. "plan_claude", "plan_codex"). Populated from PlanDraftStore on
	// task load; the convergence step reads this map and writes the merged
	// result to Plan.
	PlanDrafts map[string]string `yaml:"-" json:"planDrafts,omitempty"`
	FilePath   string            `yaml:"-" json:"filePath"`
}

func (t Task) DirName() string {
	if t.Slug == "" {
		return t.ID
	}
	return t.Slug + "-" + t.ID
}
