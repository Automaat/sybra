package task

import (
	"fmt"
	"time"
)

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

var validStatuses = map[Status]bool{
	StatusNew: true, StatusTodo: true, StatusInProgress: true,
	StatusInReview: true, StatusPlanning: true, StatusPlanReview: true,
	StatusHumanRequired: true, StatusDone: true,
}

// AllStatuses returns every valid status in display order.
func AllStatuses() []Status {
	return []Status{
		StatusNew, StatusTodo, StatusPlanning, StatusPlanReview,
		StatusInProgress, StatusInReview, StatusHumanRequired, StatusDone,
	}
}

func ValidateStatus(s string) (Status, error) {
	st := Status(s)
	if !validStatuses[st] {
		return "", fmt.Errorf("invalid status %q (valid: %v)", s, AllStatuses())
	}
	return st, nil
}

type TaskType string

const (
	TaskTypeNormal   TaskType = "normal"
	TaskTypeDebug    TaskType = "debug"
	TaskTypeResearch TaskType = "research"
)

var validTaskTypes = map[TaskType]bool{
	TaskTypeNormal: true, TaskTypeDebug: true, TaskTypeResearch: true,
}

// AllTaskTypes returns every valid task type in display order.
func AllTaskTypes() []TaskType {
	return []TaskType{TaskTypeNormal, TaskTypeDebug, TaskTypeResearch}
}

func ValidateTaskType(s string) (TaskType, error) {
	tt := TaskType(s)
	if !validTaskTypes[tt] {
		return "", fmt.Errorf("invalid task_type %q (valid: %v)", s, AllTaskTypes())
	}
	return tt, nil
}

type AgentRun struct {
	AgentID   string    `yaml:"agent_id" json:"agentId"`
	Role      string    `yaml:"role,omitempty" json:"role"` // triage, plan, eval, pr-fix, or "" for implementation
	Mode      string    `yaml:"mode" json:"mode"`
	State     string    `yaml:"state" json:"state"`
	StartedAt time.Time `yaml:"started_at" json:"startedAt"`
	CostUSD   float64   `yaml:"cost_usd,omitempty" json:"costUsd"`
	Result    string    `yaml:"result,omitempty" json:"result"`
	LogFile   string    `yaml:"log_file,omitempty" json:"logFile"`
}

type Task struct {
	ID           string     `yaml:"id" json:"id"`
	Slug         string     `yaml:"slug,omitempty" json:"slug"`
	Title        string     `yaml:"title" json:"title"`
	Status       Status     `yaml:"status" json:"status"`
	TaskType     TaskType   `yaml:"task_type,omitempty" json:"taskType"`
	AgentMode    string     `yaml:"agent_mode" json:"agentMode"`
	AllowedTools []string   `yaml:"allowed_tools" json:"allowedTools"`
	Tags         []string   `yaml:"tags" json:"tags"`
	ProjectID    string     `yaml:"project_id,omitempty" json:"projectId"`
	Branch       string     `yaml:"branch,omitempty" json:"branch"`
	PRNumber     int        `yaml:"pr_number,omitempty" json:"prNumber"`
	Issue        string     `yaml:"issue,omitempty" json:"issue"`
	StatusReason string     `yaml:"status_reason,omitempty" json:"statusReason"`
	Reviewed     bool       `yaml:"reviewed,omitempty" json:"reviewed"`
	RunRole      string     `yaml:"run_role,omitempty" json:"runRole"` // pr-fix when fixing review issues, "" for initial impl
	AgentRuns    []AgentRun `yaml:"agent_runs,omitempty" json:"agentRuns"`
	CreatedAt    time.Time  `yaml:"created_at" json:"createdAt"`
	UpdatedAt    time.Time  `yaml:"updated_at" json:"updatedAt"`

	Body     string `yaml:"-" json:"body"`
	FilePath string `yaml:"-" json:"filePath"`
}

func (t Task) DirName() string {
	if t.Slug == "" {
		return t.ID
	}
	return t.Slug + "-" + t.ID
}
