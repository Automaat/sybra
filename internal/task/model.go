package task

import "time"

type Status string

const (
	StatusNew           Status = "new"
	StatusTodo          Status = "todo"
	StatusInProgress    Status = "in-progress"
	StatusInReview      Status = "in-review"
	StatusPlanning      Status = "planning"
	StatusPlanReview    Status = "plan-review"
	StatusHumanRequired Status = "human-required"
	StatusDone          Status = "done"
)

type AgentRun struct {
	AgentID   string    `yaml:"agent_id" json:"agentId"`
	Mode      string    `yaml:"mode" json:"mode"`
	State     string    `yaml:"state" json:"state"`
	StartedAt time.Time `yaml:"started_at" json:"startedAt"`
	CostUSD   float64   `yaml:"cost_usd,omitempty" json:"costUsd"`
	Result    string    `yaml:"result,omitempty" json:"result"`
	LogFile   string    `yaml:"log_file,omitempty" json:"logFile"`
}

type Task struct {
	ID           string     `yaml:"id" json:"id"`
	Title        string     `yaml:"title" json:"title"`
	Status       Status     `yaml:"status" json:"status"`
	AgentMode    string     `yaml:"agent_mode" json:"agentMode"`
	AllowedTools []string   `yaml:"allowed_tools" json:"allowedTools"`
	Tags         []string   `yaml:"tags" json:"tags"`
	ProjectID    string     `yaml:"project_id,omitempty" json:"projectId"`
	AgentRuns    []AgentRun `yaml:"agent_runs,omitempty" json:"agentRuns"`
	CreatedAt    time.Time  `yaml:"created_at" json:"createdAt"`
	UpdatedAt    time.Time  `yaml:"updated_at" json:"updatedAt"`

	Body     string `yaml:"-" json:"body"`
	FilePath string `yaml:"-" json:"filePath"`
}
