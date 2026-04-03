package task

import "time"

type Status string

const (
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in-progress"
	StatusInReview   Status = "in-review"
	StatusDone       Status = "done"
)

type Task struct {
	ID           string    `yaml:"id" json:"id"`
	Title        string    `yaml:"title" json:"title"`
	Status       Status    `yaml:"status" json:"status"`
	AgentMode    string    `yaml:"agent_mode" json:"agentMode"`
	AllowedTools []string  `yaml:"allowed_tools" json:"allowedTools"`
	Tags         []string  `yaml:"tags" json:"tags"`
	CreatedAt    time.Time `yaml:"created_at" json:"createdAt"`
	UpdatedAt    time.Time `yaml:"updated_at" json:"updatedAt"`

	Body     string `yaml:"-" json:"body"`
	FilePath string `yaml:"-" json:"filePath"`
}
