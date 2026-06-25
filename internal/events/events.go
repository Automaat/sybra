// Package events defines Wails event name constants shared across the app.
//
//go:generate go run ../../cmd/gen-events
package events

const (
	// Task lifecycle events (emitted by watcher).
	TaskCreated = "task:created"
	TaskUpdated = "task:updated"
	TaskDeleted = "task:deleted"

	// Agent events — prefix only; append ":"+agentID to form full event name.
	AgentStatePrefix        = "agent:state:"
	AgentOutputPrefix       = "agent:output:"
	AgentErrorPrefix        = "agent:error:"
	AgentStuckPrefix        = "agent:stuck:"
	AgentConvoPrefix        = "agent:convo:"
	AgentApprovalPrefix     = "agent:approval:"
	AgentEscalationPrefix   = "agent:escalation:"
	AgentPluginErrorsPrefix = "agent:plugin_errors:"

	// Orchestrator events.
	OrchestratorState = "orchestrator:state"

	// MonitorReport fires at the end of every monitor.Service tick with the
	// snapshot of board state + detected anomalies. Payload is a
	// monitor.Report (see internal/monitor/report.go).
	MonitorReport = "monitor:report"

	// SelfMonitorReport fires at the end of every selfmonitor.Service tick
	// with the distilled health findings, verdicts, correlations, filed
	// issues, and actions taken. Payload is a selfmonitor.Report
	// (see internal/selfmonitor/report.go).
	SelfMonitorReport = "selfmonitor:report"

	// EvaluationReport fires at the end of every evaluation.Service tick with
	// the fleet scorecard (autonomy, throughput, reliability, efficiency).
	// Payload is an evaluation.Report (see internal/evaluation/scorecard.go).
	EvaluationReport = "evaluation:report"

	// Loop agent events — emitted whenever the scheduler reconciles or
	// records a new run on a loop agent. Carries no payload; consumers
	// re-list LoopAgents on receipt.
	LoopAgentUpdated = "loopagent:updated"

	// Review/PR events.
	ReviewsUpdated  = "reviews:updated"
	RenovateUpdated = "renovate:updated"

	// Notification events.
	Notification = "notification"

	// Todoist integration events.
	TodoistSynced = "todoist:synced"

	// GitHub issues events.
	IssuesUpdated = "issues:updated"

	// Background operation events — emitted by bgop.Tracker for long-running
	// operations (clone, worktree prep). Payload is a bgop.Operation.
	BgOpStarted   = "bgop:started"
	BgOpProgress  = "bgop:progress"
	BgOpCompleted = "bgop:completed"
	BgOpFailed    = "bgop:failed"

	// App lifecycle events.
	AppQuitConfirm  = "app:quit-confirm"
	StartupDegraded = "startup:degraded"

	// Provider health events — emitted by internal/provider.Checker when a
	// provider (claude, codex) flips healthy/unhealthy or a rate-limit window
	// elapses. Payload matches provider.HealthEvent.
	ProviderHealth = "provider:health"
)

// AgentState returns the agent state event name for the given agent ID.
func AgentState(id string) string { return AgentStatePrefix + id }

// AgentOutput returns the agent output event name for the given agent ID.
func AgentOutput(id string) string { return AgentOutputPrefix + id }

// AgentError returns the agent error event name for the given agent ID.
func AgentError(id string) string { return AgentErrorPrefix + id }

// AgentStuck returns the agent stuck event name for the given agent ID.
func AgentStuck(id string) string { return AgentStuckPrefix + id }

// AgentConvo returns the conversational output event name for the given agent ID.
func AgentConvo(id string) string { return AgentConvoPrefix + id }

// AgentApproval returns the tool approval event name for the given agent ID.
func AgentApproval(id string) string { return AgentApprovalPrefix + id }

// AgentEscalation returns the escalation event name for the given agent ID.
func AgentEscalation(id string) string { return AgentEscalationPrefix + id }

// AgentPluginErrors returns the plugin errors event name for the given agent ID.
func AgentPluginErrors(id string) string { return AgentPluginErrorsPrefix + id }
