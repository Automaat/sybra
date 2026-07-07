package audit

import "time"

const (
	EventTaskCreated         = "task.created"
	EventTaskStatusChanged   = "task.status_changed"
	EventTaskTamperBlessed   = "task.tamper_blessed"
	EventTaskDeleted         = "task.deleted"
	EventAgentStarted        = "agent.started"
	EventAgentCompleted      = "agent.completed"
	EventAgentFailed         = "agent.failed"
	EventTriageCompleted     = "triage.completed"
	EventTriageClassified    = "triage.classified"
	EventPlanCompleted       = "plan.completed"
	EventPlanApproved        = "plan.approved"
	EventPlanRejected        = "plan.rejected"
	EventEvalCompleted       = "eval.completed"
	EventOrchestratorStart   = "orchestrator.started"
	EventOrchestratorStop    = "orchestrator.stopped"
	EventPRConflictDetected  = "pr_monitor.conflict_detected"
	EventPRCIFailureDetected = "pr_monitor.ci_failure_detected"
	EventPRCommentsDetected  = "pr_monitor.comments_detected"
	EventPRFixAgentStarted   = "pr_monitor.fix_agent_started"
	EventPRFixExhausted      = "pr_monitor.fix_exhausted"
	EventPRMerged            = "pr_monitor.merged"
	EventPRClosed            = "pr_monitor.closed"
	EventPRAutoMerged        = "pr_monitor.auto_merged"
	// EventAutoMergeEnabled records that GitHub's native auto-merge was armed
	// on a PR (the pr_monitor.merged/auto_merged events cover the eventual
	// terminal merge itself, whichever path lands it).
	EventAutoMergeEnabled         = "pr_monitor.auto_merge_enabled"
	EventPROrphanAdopted          = "pr_monitor.orphan_adopted"
	EventPRCopilotThreadsResolved = "pr_monitor.copilot_threads_resolved"
	// EventPRConflictAutoResolved records that a conflict-recovery agent was
	// skipped because a deterministic git merge of the base branch succeeded
	// cleanly and was pushed. Data carries only pr and issue kind — never PR
	// titles, branch names, or agent output.
	EventPRConflictAutoResolved = "pr_monitor.conflict_auto_resolved"
	// EventBranchConflictAutoResolved records that a no-PR worktree-prep
	// rebase conflict was recovered autonomously by merging base into the
	// task's own branch and resuming its interrupted workflow/stage. Data
	// carries only the task ID (via the audit envelope) — never branch
	// names, commit SHAs, or agent output.
	EventBranchConflictAutoResolved = "pr_monitor.branch_conflict_auto_resolved"
	EventReviewStarted              = "review.agent_started"
	EventFixReviewStarted           = "fix_review.agent_started"
	EventReviewPublished            = "review.published"
	EventTodoistImported            = "todoist.imported"
	EventTodoistCompleted           = "todoist.completed"
	EventRenovateCIFix              = "renovate.ci_fix_started"
	EventHealthReport               = "health.report"
	EventAgentStartFailed           = "agent.start_failed"
	EventProviderGateBlocked        = "provider.gate_blocked"
	EventHumanReviewSpawned         = "human_review.spawned"
	EventHumanReviewVerdict         = "human_review.verdict"
	EventHumanReviewIssue           = "human_review.issue_filed"
	EventHumanReviewSkipped         = "human_review.skipped"
	EventExperienceRecorded         = "experience.recorded"
	EventExperienceSkipped          = "experience.skipped"
	EventExperienceInjected         = "experience.injected"

	// EventTaskLanded records a task's terminal outcome (merged/closed) with
	// queue-inclusive and work-based timing for the evaluation scorecard.
	// Emitted once when a task auto-advances to done on its PR closing.
	EventTaskLanded = "task.landed"
	// EventPRReverted records that a previously-merged PR was reverted on the
	// default branch — a change-failure signal. Reserved for revert detection.
	EventPRReverted = "pr.reverted"
	// EventAgentPermissionDenied is emitted once per auto-mode classifier denial
	// observed during a headless claude run. Batched at completion time.
	EventAgentPermissionDenied = "agent.permission_denied"
	// EventAgentSandboxDisabled is emitted once per dispatch when a task's
	// per-task Sandbox escape hatch (sandbox: false) overrides the configured
	// OS-level process-sandbox default to "off", so an operator reviewing the
	// audit log can see which tasks are running with unrestricted file-write
	// access instead of only discovering it after an incident.
	EventAgentSandboxDisabled = "agent.sandbox_disabled"

	// Codex lifecycle hook events — emitted by the sybra-cli hook fast-path
	// when codex fires its session/subagent lifecycle hooks. Distinct from the
	// stream-derived agent.* events to avoid double-counting.
	EventCodexSessionStart  = "codex.session.start"
	EventCodexSubagentStart = "codex.subagent.start"
	EventCodexSubagentStop  = "codex.subagent.stop"
	EventCodexSessionStop   = "codex.session.stop"
	// EventCodexHookFailed is written by the sybra-cli hook fast-path when
	// the receiver encounters an error (payload read, JSON mapping, audit
	// write). The Data.reason field carries a categorical label — never the
	// raw error message — so failures are observable without exposing content.
	EventCodexHookFailed = "codex.hook.failed"

	// EventLearningDigest records a successful Learning Digest generation
	// (internal/learning) with provider/model/duration/cost in Data.
	EventLearningDigest = "learning.digest"
	// EventLearningDigestFailed records a failed or malformed digest run.
	// Data.reason carries an actionable, categorical explanation; the
	// previous digest is left intact.
	EventLearningDigestFailed = "learning.digest_failed"
)

type Event struct {
	Timestamp time.Time      `json:"ts"`
	Type      string         `json:"type"`
	TaskID    string         `json:"task_id,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}
