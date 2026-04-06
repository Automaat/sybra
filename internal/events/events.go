// Package events defines Wails event name constants shared across the app.
package events

const (
	// Task lifecycle events (emitted by watcher).
	TaskCreated = "task:created"
	TaskUpdated = "task:updated"
	TaskDeleted = "task:deleted"

	// Agent events — prefix only; append ":"+agentID to form full event name.
	AgentStatePrefix  = "agent:state:"
	AgentOutputPrefix = "agent:output:"
	AgentErrorPrefix  = "agent:error:"
	AgentStuckPrefix  = "agent:stuck:"

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
