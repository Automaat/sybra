// Wails event name constants — must stay in sync with internal/events/events.go

export const TaskCreated = 'task:created'
export const TaskUpdated = 'task:updated'
export const TaskDeleted = 'task:deleted'

export const AgentStatePrefix = 'agent:state:'
export const AgentOutputPrefix = 'agent:output:'
export const AgentErrorPrefix = 'agent:error:'

export const OrchestratorState = 'orchestrator:state'
export const ReviewsUpdated = 'reviews:updated'
export const Notification = 'notification'
export const AppQuitConfirm = 'app:quit-confirm'

export const agentState = (id: string) => `${AgentStatePrefix}${id}`
export const agentOutput = (id: string) => `${AgentOutputPrefix}${id}`
export const agentError = (id: string) => `${AgentErrorPrefix}${id}`
