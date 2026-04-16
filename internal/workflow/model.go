package workflow

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

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
// Priority breaks ties when multiple definitions match the same event: higher
// wins, zero is the default. Ties at the same priority fall back to
// alphabetical order by workflow ID for determinism.
type Trigger struct {
	On         string      `yaml:"on" json:"on"` // "task.created", "task.status_changed", "pr.event"
	Priority   int         `yaml:"priority,omitempty" json:"priority"`
	Conditions []Condition `yaml:"conditions,omitempty" json:"conditions"`

	// Position stores x,y for the graph editor (not used by engine).
	Position *Position `yaml:"position,omitempty" json:"position"`
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
	StepRunAgent            StepType = "run_agent"
	StepWaitHuman           StepType = "wait_human"
	StepSetStatus           StepType = "set_status"
	StepCondition           StepType = "condition"
	StepShell               StepType = "shell"
	StepEnsurePRClosesIssue StepType = "ensure_pr_closes_issue"
	StepVerifyCommits       StepType = "verify_commits"
	StepLinkPRAndReview     StepType = "link_pr_and_review"
	StepEvaluate            StepType = "evaluate"
	StepRequireSidecar      StepType = "require_sidecar"
)

// Step is one node in the workflow graph.
type Step struct {
	ID       string       `yaml:"id" json:"id"`
	Name     string       `yaml:"name" json:"name"`
	Type     StepType     `yaml:"type" json:"type"`
	Config   StepConfig   `yaml:"config" json:"config"`
	Next     []Transition `yaml:"next,omitempty" json:"next"`
	Parallel []Step       `yaml:"parallel,omitempty" json:"parallel"`

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
	Provider      string   `yaml:"provider,omitempty" json:"provider"` // "", "claude", "codex", "cross"
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

	// run_agent: treat the step as complete when the task status transitions
	// to this value. Required for interactive/conversational agents that
	// never exit on their own — the workflow advances on status change
	// instead of process exit.
	WaitForStatus string `yaml:"wait_for_status,omitempty" json:"waitForStatus"`

	// shell
	Command string `yaml:"command,omitempty" json:"command"`
	Dir     string `yaml:"dir,omitempty" json:"dir"`

	// require_sidecar: which sidecar must be non-empty for the step to pass.
	// Valid values: "plan_critique", "code_review".
	Sidecar string `yaml:"sidecar,omitempty" json:"sidecar"`
}

const maxRetries = 10

// Validate checks the definition for configuration errors.
func (d *Definition) Validate() error {
	for i := range d.Steps {
		s := &d.Steps[i]
		if s.Config.MaxRetries > maxRetries {
			return fmt.Errorf("step %q: max_retries %d exceeds limit %d", s.ID, s.Config.MaxRetries, maxRetries)
		}
	}
	if err := d.ValidateFields(); err != nil {
		return err
	}
	return nil
}

// ValidateFields returns an error listing any trigger or transition field
// references that the engine will never populate, plus any enum-value
// comparisons that pick values outside the known enum set. Catches two
// classes of dead workflow condition at load/save time:
//
//  1. A trigger on "project.type" with no caller supplying that key (the
//     auto-merge dead-code shape).
//  2. A trigger on "pr.issue_kind" comparing against "ci-failure" (dash)
//     while the constant emits "ci_failure" (underscore) — the original
//     dispatch-mismatch shape.
func (d *Definition) ValidateFields() error {
	acc := fieldValidationAcc{
		unknown:      map[string]bool{},
		badValues:    map[string][]string{},
		badOperators: map[string][]string{},
	}
	for i := range d.Trigger.Conditions {
		collectUnknownCondition(&d.Trigger.Conditions[i], &acc)
	}
	for i := range d.Steps {
		collectUnknownStepFields(&d.Steps[i], &acc)
	}
	if len(acc.unknown) == 0 && len(acc.badValues) == 0 && len(acc.badOperators) == 0 {
		return nil
	}
	var parts []string
	if len(acc.unknown) > 0 {
		names := make([]string, 0, len(acc.unknown))
		for k := range acc.unknown {
			names = append(names, k)
		}
		sort.Strings(names)
		parts = append(parts, "unknown field(s): "+strings.Join(names, ", "))
	}
	if len(acc.badValues) > 0 {
		fields := make([]string, 0, len(acc.badValues))
		for k := range acc.badValues {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		for _, f := range fields {
			vals := acc.badValues[f]
			sort.Strings(vals)
			parts = append(parts, fmt.Sprintf("invalid %s value(s): %s", f, strings.Join(vals, ",")))
		}
	}
	if len(acc.badOperators) > 0 {
		fields := make([]string, 0, len(acc.badOperators))
		for k := range acc.badOperators {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		for _, f := range fields {
			ops := acc.badOperators[f]
			sort.Strings(ops)
			parts = append(parts, fmt.Sprintf(
				"operator %s not allowed on enum field %s (use equals/in)",
				strings.Join(ops, ","), f))
		}
	}
	return fmt.Errorf("workflow %q: %s", d.ID, strings.Join(parts, "; "))
}

// fieldValidationAcc accumulates ValidateFields results while walking the
// trigger and step tree. Grouped to keep the recursive collectors' signatures
// from growing a new parameter every time a new class of error gets added.
type fieldValidationAcc struct {
	unknown      map[string]bool
	badValues    map[string][]string
	badOperators map[string][]string
}

func collectUnknownCondition(c *Condition, acc *fieldValidationAcc) {
	if !isKnownField(c.Field) {
		acc.unknown[c.Field] = true
		return
	}
	if checkEnumOperator(c.Field, c.Operator) {
		acc.badOperators[c.Field] = append(acc.badOperators[c.Field], c.Operator)
		return
	}
	if bad := checkEnumValue(c.Field, c.Operator, c.Value); len(bad) > 0 {
		acc.badValues[c.Field] = append(acc.badValues[c.Field], bad...)
	}
}

func collectUnknownStepFields(s *Step, acc *fieldValidationAcc) {
	if s.Config.Check != nil {
		collectUnknownCondition(s.Config.Check, acc)
	}
	for i := range s.Next {
		if s.Next[i].When != nil {
			collectUnknownCondition(s.Next[i].When, acc)
		}
	}
	for i := range s.Parallel {
		collectUnknownStepFields(&s.Parallel[i], acc)
	}
}

// Transition defines an edge from one step to another.
type Transition struct {
	When *Condition `yaml:"when,omitempty" json:"when"` // nil = default/fallback
	GoTo string     `yaml:"goto" json:"goto"`           // step ID; "" = end workflow
}
