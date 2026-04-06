package audit

import "time"

const (
	EventTaskCreated         = "task.created"
	EventTaskStatusChanged   = "task.status_changed"
	EventTaskDeleted         = "task.deleted"
	EventAgentStarted        = "agent.started"
	EventAgentCompleted      = "agent.completed"
	EventAgentFailed         = "agent.failed"
	EventTriageCompleted     = "triage.completed"
	EventPlanCompleted       = "plan.completed"
	EventPlanApproved        = "plan.approved"
	EventPlanRejected        = "plan.rejected"
	EventEvalCompleted       = "eval.completed"
	EventOrchestratorStart   = "orchestrator.started"
	EventOrchestratorStop    = "orchestrator.stopped"
	EventPRConflictDetected  = "pr_monitor.conflict_detected"
	EventPRCIFailureDetected = "pr_monitor.ci_failure_detected"
	EventPRFixAgentStarted   = "pr_monitor.fix_agent_started"
	EventPRMerged            = "pr_monitor.merged"
	EventPRClosed            = "pr_monitor.closed"
	EventPRAutoMerged        = "pr_monitor.auto_merged"
	EventReviewStarted       = "review.agent_started"
	EventReviewPublished     = "review.published"
	EventTodoistImported     = "todoist.imported"
	EventTodoistCompleted    = "todoist.completed"
	EventRenovateCIFix       = "renovate.ci_fix_started"
)

type Event struct {
	Timestamp time.Time      `json:"ts"`
	Type      string         `json:"type"`
	TaskID    string         `json:"task_id,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}
