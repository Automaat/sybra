// Package events defines Wails event name constants shared across the app.
package events

const (
	// Task lifecycle events (emitted by watcher).
	TaskCreated = "task:created"
	TaskUpdated = "task:updated"
	TaskDeleted = "task:deleted"

	// Agent events — prefix only; append ":"+agentID to form full event name.
	AgentStatePrefix      = "agent:state:"
	AgentOutputPrefix     = "agent:output:"
	AgentErrorPrefix      = "agent:error:"
	AgentStuckPrefix      = "agent:stuck:"
	AgentConvoPrefix      = "agent:convo:"
	AgentApprovalPrefix   = "agent:approval:"
	AgentEscalationPrefix = "agent:escalation:"

	// Orchestrator events.
	OrchestratorState = "orchestrator:state"

	// Review/PR events.
	ReviewsUpdated = "reviews:updated"

	// Notification events.
	Notification = "notification"

	// Todoist integration events.
	TodoistSynced = "todoist:synced"

	// GitHub issues events.
	IssuesUpdated = "issues:updated"

	// App lifecycle events.
	AppQuitConfirm = "app:quit-confirm"
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
