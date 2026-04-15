package task

import (
	"fmt"
	"time"

	"github.com/Automaat/synapse/internal/workflow"
)

type Status string

const (
	StatusNew            Status = "new"
	StatusTodo           Status = "todo"
	StatusInProgress     Status = "in-progress"
	StatusInReview       Status = "in-review"
	StatusPlanning       Status = "planning"
	StatusPlanReview     Status = "plan-review"
	StatusTesting        Status = "testing"
	StatusTestPlanReview Status = "test-plan-review"
	StatusHumanRequired  Status = "human-required"
	StatusDone           Status = "done"
)

var validStatuses = map[Status]bool{
	StatusNew: true, StatusTodo: true, StatusInProgress: true,
	StatusInReview: true, StatusPlanning: true, StatusPlanReview: true,
	StatusTesting: true, StatusTestPlanReview: true,
	StatusHumanRequired: true, StatusDone: true,
}

// AllStatuses returns every valid status in display order.
func AllStatuses() []Status {
	return []Status{
		StatusNew, StatusTodo, StatusPlanning, StatusPlanReview,
		StatusInProgress, StatusInReview,
		StatusTesting, StatusTestPlanReview,
		StatusHumanRequired, StatusDone,
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
	// TaskTypeChat is a synthetic task created for interactive chat sessions.
	// Hidden from the task list UI and skipped by restart-stale/watchdog.
	TaskTypeChat TaskType = "chat"
)

var validTaskTypes = map[TaskType]bool{
	TaskTypeNormal: true, TaskTypeDebug: true, TaskTypeResearch: true, TaskTypeChat: true,
}

// AllTaskTypes returns every valid task type in display order.
func AllTaskTypes() []TaskType {
	return []TaskType{TaskTypeNormal, TaskTypeDebug, TaskTypeResearch, TaskTypeChat}
}

func ValidateTaskType(s string) (TaskType, error) {
	tt := TaskType(s)
	if !validTaskTypes[tt] {
		return "", fmt.Errorf("invalid task_type %q (valid: %v)", s, AllTaskTypes())
	}
	return tt, nil
}

const (
	AgentModeHeadless    = "headless"
	AgentModeInteractive = "interactive"
)

var validAgentModes = map[string]bool{
	AgentModeHeadless: true, AgentModeInteractive: true,
}

// AllAgentModes returns every valid agent mode in display order.
func AllAgentModes() []string {
	return []string{AgentModeHeadless, AgentModeInteractive}
}

// ValidateAgentMode rejects unknown agent modes. Empty strings are rejected
// here; callers that need to allow "unset" (e.g. parser legacy compat) must
// guard the empty case before calling.
func ValidateAgentMode(s string) (string, error) {
	if !validAgentModes[s] {
		return "", fmt.Errorf("invalid agent_mode %q (valid: %v)", s, AllAgentModes())
	}
	return s, nil
}

type AgentRun struct {
	AgentID   string    `yaml:"agent_id" json:"agentId"`
	Role      string    `yaml:"role,omitempty" json:"role"` // triage, plan, eval, pr-fix, or "" for implementation
	Mode      string    `yaml:"mode" json:"mode"`
	Provider  string    `yaml:"provider,omitempty" json:"provider,omitempty"`
	State     string    `yaml:"state" json:"state"`
	StartedAt time.Time `yaml:"started_at" json:"startedAt"`
	CostUSD   float64   `yaml:"cost_usd,omitempty" json:"costUsd"`
	Result    string    `yaml:"result,omitempty" json:"result"`
	LogFile   string    `yaml:"log_file,omitempty" json:"logFile"`
}

type Task struct {
	ID           string   `yaml:"id" json:"id"`
	Slug         string   `yaml:"slug,omitempty" json:"slug"`
	Title        string   `yaml:"title" json:"title"`
	Status       Status   `yaml:"status" json:"status"`
	TaskType     TaskType `yaml:"task_type,omitempty" json:"taskType"`
	AgentMode    string   `yaml:"agent_mode" json:"agentMode"`
	AllowedTools []string `yaml:"allowed_tools" json:"allowedTools"`
	Tags         []string `yaml:"tags" json:"tags"`
	ProjectID    string   `yaml:"project_id,omitempty" json:"projectId"`
	Branch       string   `yaml:"branch,omitempty" json:"branch"`
	PRNumber     int      `yaml:"pr_number,omitempty" json:"prNumber"`
	Issue        string   `yaml:"issue,omitempty" json:"issue"`
	StatusReason string   `yaml:"status_reason,omitempty" json:"statusReason"`
	Reviewed     bool     `yaml:"reviewed,omitempty" json:"reviewed"`
	RunRole      string   `yaml:"run_role,omitempty" json:"runRole"` // pr-fix when fixing review issues, "" for initial impl
	TodoistID    string     `yaml:"todoist_id,omitempty" json:"todoistId"`
	DueDate      *time.Time `yaml:"due_date,omitempty" json:"dueDate,omitempty"`
	// RequirePermissions overrides the system default when set.
	// nil = use system default (true). false = opt out (--dangerously-skip-permissions).
	RequirePermissions *bool               `yaml:"require_permissions,omitempty" json:"requirePermissions,omitempty"`
	AgentRuns          []AgentRun          `yaml:"agent_runs,omitempty" json:"agentRuns"`
	Workflow           *workflow.Execution `yaml:"workflow,omitempty" json:"workflow"`
	CreatedAt          time.Time           `yaml:"created_at" json:"createdAt"`
	UpdatedAt          time.Time           `yaml:"updated_at" json:"updatedAt"`

	Body         string `yaml:"-" json:"body"`
	Plan         string `yaml:"-" json:"plan,omitempty"`
	PlanCritique string `yaml:"-" json:"planCritique,omitempty"`
	FilePath     string `yaml:"-" json:"filePath"`
}

func (t Task) DirName() string {
	if t.Slug == "" {
		return t.ID
	}
	return t.Slug + "-" + t.ID
}
