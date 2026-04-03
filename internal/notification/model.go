package notification

// Level represents notification severity.
type Level string

const (
	LevelInfo    Level = "info"
	LevelSuccess Level = "success"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
)

// Notification is an ephemeral in-app notification.
type Notification struct {
	ID        string `json:"id"`
	Level     Level  `json:"level"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	TaskID    string `json:"taskId,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
	CreatedAt string `json:"createdAt"`
}
