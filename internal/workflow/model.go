package workflow

import "time"

// Definition is a declarative workflow stored as YAML.
type Definition struct {
	ID          string    `yaml:"id" json:"id"`
	Name        string    `yaml:"name" json:"name"`
	Description string    `yaml:"description,omitempty" json:"description"`
	Trigger     Trigger   `yaml:"trigger" json:"trigger"`
	Steps       []Step    `yaml:"steps" json:"steps"`
	Builtin     bool      `yaml:"builtin,omitempty" json:"builtin"`
	CreatedAt   time.Time `yaml:"created_at,omitempty" json:"createdAt"`
	UpdatedAt   time.Time `yaml:"updated_at,omitempty" json:"updatedAt"`
}

// StepByID returns the step with the given ID, or nil.
func (d *Definition) StepByID(id string) *Step {
	for i := range d.Steps {
		if d.Steps[i].ID == id {
			return &d.Steps[i]
		}
		// Check parallel sub-steps.
		for j := range d.Steps[i].Parallel {
			if d.Steps[i].Parallel[j].ID == id {
				return &d.Steps[i].Parallel[j]
			}
		}
	}
	return nil
}

// FirstStep returns the first step, or nil if empty.
func (d *Definition) FirstStep() *Step {
	if len(d.Steps) == 0 {
		return nil
	}
	return &d.Steps[0]
}

// Trigger defines when a workflow activates.
type Trigger struct {
	On         string      `yaml:"on" json:"on"` // "task.created", "task.status_changed", "pr.event"
	Conditions []Condition `yaml:"conditions,omitempty" json:"conditions"`
}

// Condition is a field-operator-value check.
type Condition struct {
	Field    string `yaml:"field" json:"field"`       // "task.tags", "task.status", "task.agent_mode"
	Operator string `yaml:"operator" json:"operator"` // "equals", "not_equals", "contains", "not_contains", "exists"
	Value    string `yaml:"value" json:"value"`
}

// StepType enumerates the kinds of workflow steps.
type StepType string

const (
	StepRunAgent  StepType = "run_agent"
	StepWaitHuman StepType = "wait_human"
	StepSetStatus StepType = "set_status"
	StepCondition StepType = "condition"
	StepShell     StepType = "shell"
	StepParallel  StepType = "parallel"
)

// Step is one node in the workflow graph.
type Step struct {
	ID     string       `yaml:"id" json:"id"`
	Name   string       `yaml:"name" json:"name"`
	Type   StepType     `yaml:"type" json:"type"`
	Config StepConfig   `yaml:"config" json:"config"`
	Next   []Transition `yaml:"next,omitempty" json:"next"`

	// Parallel holds sub-steps for StepParallel type.
	Parallel []Step `yaml:"parallel,omitempty" json:"parallel"`

	// Position stores x,y for the graph editor (not used by engine).
	Position *Position `yaml:"position,omitempty" json:"position"`
}

// Position stores node coordinates for the visual editor.
type Position struct {
	X float64 `yaml:"x" json:"x"`
	Y float64 `yaml:"y" json:"y"`
}

// StepConfig holds type-specific configuration.
// Only fields relevant to the step type are populated.
type StepConfig struct {
	// run_agent
	Role          string   `yaml:"role,omitempty" json:"role"`
	Mode          string   `yaml:"mode,omitempty" json:"mode"`
	Model         string   `yaml:"model,omitempty" json:"model"`
	Prompt        string   `yaml:"prompt,omitempty" json:"prompt"`
	AllowedTools  []string `yaml:"allowed_tools,omitempty" json:"allowedTools"`
	NeedsWorktree bool     `yaml:"needs_worktree,omitempty" json:"needsWorktree"`

	// wait_human
	HumanActions []string `yaml:"human_actions,omitempty" json:"humanActions"`

	// set_status
	Status       string `yaml:"status,omitempty" json:"status"`
	StatusReason string `yaml:"status_reason,omitempty" json:"statusReason"`

	// condition
	Check *Condition `yaml:"check,omitempty" json:"check"`

	// run_agent: retry + reuse
	MaxRetries int  `yaml:"max_retries,omitempty" json:"maxRetries"`
	ReuseAgent bool `yaml:"reuse_agent,omitempty" json:"reuseAgent"`

	// shell
	Command string `yaml:"command,omitempty" json:"command"`
	Dir     string `yaml:"dir,omitempty" json:"dir"`
}

// Transition defines an edge from one step to another.
type Transition struct {
	When *Condition `yaml:"when,omitempty" json:"when"` // nil = default/fallback
	GoTo string     `yaml:"goto" json:"goto"`           // step ID; "" = end workflow
}
