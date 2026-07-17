package task

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/workflow"
)

// Status is a task's position in its lifecycle (see the pipeline diagram in
// the root CLAUDE.md). Transitions are enforced by the workflow engine and
// callers should validate untrusted input with ValidateStatus rather than
// casting a string directly.
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

// ValidateStatus parses s into a Status, returning an error naming every
// valid status if s does not match one of the known constants.
func ValidateStatus(s string) (Status, error) {
	st := Status(s)
	if !validStatuses[st] {
		return "", fmt.Errorf("invalid status %q (valid: %v)", s, AllStatuses())
	}
	return st, nil
}

// Priority is a task's dispatch priority. PriorityNone (the empty string) is
// treated as the lowest priority, distinct from an unset/invalid value.
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

// ValidatePriority parses s into a Priority, returning an error if s does
// not match "", "low", "medium", "high", or "urgent".
func ValidatePriority(s string) (Priority, error) {
	p := Priority(s)
	if !validPriorities[p] {
		return "", fmt.Errorf("invalid priority %q (valid: none, low, medium, high, urgent)", s)
	}
	return p, nil
}

// TaskType distinguishes a task's role for agents beyond its lifecycle
// Status — e.g. TaskTypeChat and TaskTypeUmbrella are synthetic types that
// run no agent of their own and are excluded from normal dispatch.
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

// ValidateTaskType parses s into a TaskType, returning an error naming every
// valid task type if s does not match one of the known constants.
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

// AllReasoningEfforts returns every valid reasoning effort level. This is the
// common subset accepted by all providers (codex model_reasoning_effort and the
// claude/copilot --effort flag).
func AllReasoningEfforts() []string { return []string{"low", "medium", "high", "xhigh"} }

var validReasoningEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true}

// ValidateReasoningEffort accepts the empty string (model default) or one of the
// supported levels. Static allowlist — offline-safe, no provider probe.
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
func AllAgentProviders() []string { return providerid.All() }

// ValidateAgentProvider accepts the empty string (unset/default) or one of the
// providers Sybra can dispatch. It is used for handoff provenance, not the
// task's execution mode.
func ValidateAgentProvider(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !providerid.IsKnown(s) {
		return "", fmt.Errorf("invalid provider %q (valid: %s, or empty)", s, providerid.List())
	}
	return s, nil
}

// RunOutcomeSuccess and RunOutcomeFailure are the values AgentRun.Outcome
// takes once a run reaches a definitive terminal result. Left empty for runs
// that are still in flight or ended in an inconclusive stall (signal kill,
// stop-before-result, provider rate limit) that is eligible for retry rather
// than a real success or failure.
const (
	RunOutcomeSuccess = "success"
	RunOutcomeFailure = "failure"
)

// AgentRun records one dispatch of an agent process against a task: what was
// asked, which provider/model ran it, how it concluded, and the metadata
// downstream deterministic routing (test outcome, tamper detection, A/B
// evaluation) reads back off the run. A Task accumulates these in
// Task.AgentRuns across its lifetime, most-recent last.
type AgentRun struct {
	AgentID  string `json:"agentId"`
	Role     string `json:"role"` // triage, plan, eval, pr-fix, or "" for implementation
	Mode     string `json:"mode"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// ExperimentID/VariantID capture deterministic A/B assignment selected
	// before the run started.
	ExperimentID    string `json:"experimentId,omitempty"`
	VariantID       string `json:"variantId,omitempty"`
	AssignmentUnit  string `json:"assignmentUnit,omitempty"`
	AssignmentKey   string `json:"assignmentKey,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// SkillExecutionMode records how a mandatory workflow skill actually ran:
	// native invocation, injected SKILL.md, bundled fallback, or unavailable.
	// Empty means the run had no mandatory workflow skill.
	SkillExecutionMode string `json:"skillExecutionMode,omitempty"`
	State              string `json:"state"`
	// Outcome records the terminal result the completion handler actually
	// observed (RunOutcomeSuccess/RunOutcomeFailure), independent of State
	// ("stopped" covers both a clean finish and a failed one) and independent
	// of Result (truncated to maxResultLen, so its presence/absence cannot
	// tell success from failure either). Empty means the run never reached a
	// definitive terminal outcome (still running, or stalled/rate-limited and
	// due for retry) — callers must not infer success from Outcome == "".
	Outcome string `json:"outcome,omitempty"`
	// EscalationReason records the guardrail reason that stopped the run
	// ("cost" or "turns"). Empty for ordinary completions.
	EscalationReason string    `json:"escalationReason,omitempty"`
	StartedAt        time.Time `json:"startedAt"`
	CostUSD          float64   `json:"costUsd"`
	PremiumRequests  float64   `json:"premiumRequests,omitempty"`
	Prompt           string    `json:"prompt,omitempty"`
	Result           string    `json:"result"`
	OneShot          bool      `json:"oneShot,omitempty"`
	// Verdict holds the parsed decision for human-review runs ("human" or
	// "sybra_bug"). Extracted from live agent output at completion time so
	// it survives Result truncation.
	Verdict string `json:"verdict,omitempty"`
	// VerdictRendered is set to true after onComplete has successfully applied
	// all side-effects for this run (note appended, issue filed, local task
	// created). Used by verdictAlreadyRendered as the durable rendered-marker
	// instead of body-text patterns which can collide with user content.
	VerdictRendered bool   `json:"verdictRendered,omitempty"`
	LogFile         string `json:"logFile"`
	SessionID       string `json:"sessionId,omitempty"`
	// ProtocolViolation records deterministic workflow-level contract failures
	// for this run. Test routing uses it to avoid counting a bad verifier report
	// as an implementation failure across later workflow executions.
	ProtocolViolation string `json:"protocolViolation,omitempty"`
	// TestOutcome records the deterministic classification of a test-runner run
	// (product_bug, infra_failure, ambiguous_requirement, etc.). The testing
	// router counts only product_bug outcomes against the implementation budget.
	TestOutcome string `json:"testOutcome,omitempty"`
	// TestFailureFingerprint is a stable hash of the grounded failure report.
	// Repeated fingerprints mean the same repro survived a fix attempt and should
	// get targeted human/local reproduction instead of another broad retry.
	TestFailureFingerprint string `json:"testFailureFingerprint,omitempty"`
	// HeadSHA is the worktree HEAD commit at this run's completion — what the
	// agent left on the branch. Compared against the merged PR head to detect
	// human edits after the agent (merged_with_edits) and measure edit distance.
	HeadSHA string `json:"headSha,omitempty"`
}

// Task is the in-memory representation of a task markdown file: YAML
// frontmatter (everything but Body) plus the GFM markdown Body. Store parses
// and marshals it to/from tasks/<id>.md; planning/review/critique content
// lives in separate sidecar files (see Store.Plans, Store.PlanContracts,
// etc.) and is populated onto these fields only on load.
type Task struct {
	ID           string   `json:"id"`
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	Status       Status   `json:"status"`
	TaskType     TaskType `json:"taskType"`
	AgentMode    string   `json:"agentMode"`
	AllowedTools []string `json:"allowedTools"`
	Tags         []string `json:"tags"`
	ProjectID    string   `json:"projectId"`
	Branch       string   `json:"branch"`
	// WorktreeDir, when non-empty, makes Sybra adopt an externally-created git
	// worktree at this path instead of creating its own under
	// ~/.sybra/worktrees. Used for handoff from tools like Orca: every agent
	// for this task runs in this directory, and Sybra never rebases, force-
	// pushes, or removes it — the external tool owns its lifecycle. Empty =
	// Sybra-managed worktree (default).
	WorktreeDir string `json:"worktreeDir,omitempty"`
	PRNumber    int    `json:"prNumber"`
	// Issue is the canonical originating GitHub issue this task implements —
	// set once at creation (or by auto-sources like the GitHub poller and
	// umbrella expansion) and consumed verbatim by execEnsurePRClosesIssue to
	// append "Closes <url>" to the task's PR body, by findActiveDuplicate for
	// dedup, and by the umbrella gate/DAG for state tracking. Never overwrite
	// this after creation to attach an unrelated reference — use RefIssue.
	Issue        string `json:"issue"`
	StatusReason string `json:"statusReason"`
	// HandoffSourceProvider records which local agent provider produced the
	// work before a handoff skipped directly into review/testing/PR. Workflow
	// steps with provider=cross use it when there is no Sybra-authored run
	// history to flip from.
	HandoffSourceProvider string `json:"handoffSourceProvider,omitempty"`
	// BlockedByIssue stores the URL of the GitHub issue that put the task
	// into status=blocked. Set by the human-review automation when it
	// concludes the human-required transition was caused by a Sybra bug.
	BlockedByIssue string `json:"blockedByIssue,omitempty"`
	// UmbrellaIssue links this task to the ☂️ umbrella issue it was expanded
	// from. Set on child tasks; empty for standalone tasks. The orchestrator's
	// dependency gate reads it with DependsOn to decide when a child may leave
	// `blocked`.
	UmbrellaIssue string `json:"umbrellaIssue,omitempty"`
	// RefIssue is an ad-hoc reference URL attached after creation — e.g. a
	// related finding or duplicate noted while diagnosing why a task was
	// stuck. Purely informational: unlike Issue, nothing reads RefIssue to
	// drive PR auto-close, dedup, or umbrella state.
	RefIssue string `json:"refIssue,omitempty"`
	// DependsOn lists the issue refs (full github.com issue/PR URL or
	// owner/repo#n shorthand) this task waits on — resolved by issue ref only,
	// not task IDs. While the task is `blocked`, the gate holds it until every
	// referenced task has reached `done`; an empty list releases immediately.
	// Used only by umbrella child tasks.
	DependsOn []string `json:"dependsOn,omitempty"`
	Reviewed  bool     `json:"reviewed"`
	RunRole   string   `json:"runRole"` // pr-fix when fixing review issues, "" for initial impl
	// SupervisorSteer is a one-shot corrective message left by the watchdog's
	// headless nudge: it stops a looping headless agent (which has no mid-stream
	// channel) and persists the steer here so the recovery loop re-dispatches
	// the resumed agent with the correction prepended to its prompt. Recovery
	// clears it after consuming it exactly once. Empty for tasks with no pending
	// nudge.
	SupervisorSteer string `json:"supervisorSteer,omitempty"`
	// ReviewPhase tracks where an inbound PR-review task (tag `review`) sits in
	// the review lifecycle: reviewing → drafted → awaiting-author →
	// needs-approval → approved (plus `manual` for small PRs punted to the
	// human). Computed by the PR poller; drives the board's PR Reviews lane.
	// Empty for non-review tasks.
	ReviewPhase string `json:"reviewPhase,omitempty"`
	// Bounds automated re-review per PR commit: the durable backstop against a
	// re-dispatch loop (#2164 spent 112 reviews on one unchanged commit).
	ReviewedHeadSHA      string `json:"reviewedHeadSha,omitempty"`
	ReviewedHeadAttempts int    `json:"reviewedHeadAttempts,omitempty"`
	// PRPhase tracks where an outbound own-PR task (status in-review/ready-review,
	// not tag `review`) sits in its lifecycle: draft → building → fixing →
	// changes-requested → awaiting-approval → approved. Computed by the PR poller;
	// drives the phase glyph on the board's In Review cards. Empty for tasks
	// without an own PR (and for inbound review tasks, which use ReviewPhase).
	PRPhase   string     `json:"prPhase,omitempty"`
	TodoistID string     `json:"todoistId"`
	Priority  Priority   `json:"priority,omitempty"`
	DueDate   *time.Time `json:"dueDate,omitempty"`
	ClosedAt  *time.Time `json:"closedAt,omitempty"`
	// Outcome records how a task's own PR concluded: "merged", "merged_with_edits",
	// "closed", or "reverted". Stamped by the PR monitor when the task auto-advances
	// to done (and updated to "reverted" by the revert scanner). Empty for tasks
	// that never produced a PR. Feeds the evaluation scorecard.
	Outcome string `json:"outcome,omitempty"`
	// MergeCommit is the default-branch commit a merged PR produced, recorded at
	// landing so the revert scanner can detect a later revert of it.
	MergeCommit string `json:"mergeCommit,omitempty"`
	// MaxTurns overrides the global agent turn limit for this task.
	// Zero means "use global default".
	MaxTurns int `json:"maxTurns,omitempty"`
	// RequirePermissions overrides the system default when set.
	// nil = use system default (true). false = opt out (--dangerously-skip-permissions).
	RequirePermissions *bool `json:"requirePermissions,omitempty"`
	// HeadlessPermissionMode overrides the permission posture for headless claude
	// runs on this task. "auto" emits --permission-mode auto; "bypass" keeps
	// --dangerously-skip-permissions. Empty = use config default.
	HeadlessPermissionMode string `json:"headlessPermissionMode,omitempty"`
	// ForkSubagent enables CLAUDE_CODE_FORK_SUBAGENT=1 for this task's headless
	// agent, allowing a single prompt to spawn parallel subagent runs. Trades
	// higher token cost for reduced wall-clock time on multi-part prompts.
	ForkSubagent bool `json:"forkSubagent,omitempty"`
	// Sandbox is an escape hatch for the system default OS-level sandbox
	// posture (see config.AgentDefaults.SandboxMode) for this task's agent
	// processes. nil or true = use system default. false = disable the
	// sandbox-exec wrap entirely for this task's agents. Setting true does
	// NOT tighten posture beyond the system default (ResolveSandboxMode only
	// treats Sandbox=false as meaningful).
	Sandbox *bool `json:"sandbox,omitempty"`
	// ReasoningEffort sets the reasoning level for this task's agents
	// (low/medium/high/xhigh). Empty = model default. Applied across providers:
	// codex via -c model_reasoning_effort=<v>, claude and copilot via --effort.
	// The value set is the common subset all three CLIs accept.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// TestingCycleStartedAt marks the start of the current testing cycle. It is
	// set when a human re-dispatches the task after a human-required escalation,
	// so route_test_result can exclude test-runner runs from prior cycles when
	// counting toward TestingMaxAttempts. Nil means no re-dispatch has occurred
	// and all test-runner runs count (correct for first-ever cycles).
	TestingCycleStartedAt *time.Time          `json:"testingCycleStartedAt,omitempty"`
	AgentRuns             []AgentRun          `json:"agentRuns"`
	Workflow              *workflow.Execution `json:"workflow,omitempty"`
	CreatedAt             time.Time           `json:"createdAt"`
	UpdatedAt             time.Time           `json:"updatedAt"`
	// StatusChangedAt marks the last time Status actually transitioned, as
	// opposed to UpdatedAt which is bumped by any field write (tags, audit
	// sidecars, status_reason, ...). Detectors that need to know "how long
	// has this task been in its current status" (e.g. detectLostAgents'
	// dispatch-latency grace window) must key off this field, not UpdatedAt —
	// see internal/monitor/detector.go.
	StatusChangedAt time.Time `json:"statusChangedAt"`

	AssignedNode    string     `json:"assignedNode,omitempty"`
	NodeOverride    string     `json:"nodeOverride,omitempty"`
	MirrorRev       int64      `json:"mirrorRev,omitempty"`
	MirrorUpdatedAt *time.Time `json:"mirrorUpdatedAt,omitempty"`

	Body         string `json:"body"`
	Plan         string `json:"plan,omitempty"`
	PlanContract string `json:"planContract,omitempty"`
	PlanCritique string `json:"planCritique,omitempty"`
	// Planning sidecars. Plan is the human-readable compact plan; PlanContract
	// is the machine-validated JSON contract consumed by implementation agents.
	// The remaining sidecars hold review/evidence material.
	PlanResearch  string `json:"planResearch,omitempty"`
	PlanDecisions string `json:"planDecisions,omitempty"`
	PlanBrief     string `json:"planBrief,omitempty"`
	CodeReview    string `json:"codeReview,omitempty"`
	// PlanDrafts holds per-provider raw plan outputs during dual- (or N-)
	// provider planning. Keys are typically the parallel child step ID
	// (e.g. "plan_claude", "plan_codex"). Populated from PlanDraftStore on
	// task load; the convergence step reads this map and writes the merged
	// result to Plan.
	PlanDrafts map[string]string `json:"planDrafts,omitempty"`
	FilePath   string            `json:"filePath"`
	// TamperFlagged reports whether this task is parked at human-required
	// pending a tamper bless. Derived from Status/StatusReason (never
	// persisted) so the frontend doesn't need to duplicate
	// workflow.TamperFlaggedReasonPrefix to decide whether to show the bless
	// action. Recomputed on every load/update — see taskFromFrontmatter and
	// Store.UpdateWithPrev.
	TamperFlagged bool `json:"tamperFlagged"`
}

// DirName returns the human-readable worktree/artifact directory name for
// t: "<slug>-<id>", or bare ID when no slug has been assigned yet.
func (t Task) DirName() string {
	if t.Slug == "" {
		return t.ID
	}
	return t.Slug + "-" + t.ID
}

// isTamperFlagged reports whether a task's status/status_reason combination
// represents an unblessed tamper flag. Single source of truth for both the
// derived Task.TamperFlagged field and BlessTampering's precondition check.
func isTamperFlagged(status Status, statusReason string) bool {
	return status == StatusHumanRequired && workflow.IsTamperFlaggedReason(statusReason)
}
