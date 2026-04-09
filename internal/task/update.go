package task

import (
	"fmt"
	"strings"

	"github.com/Automaat/synapse/internal/workflow"
)

// Update carries optional field changes for Store.Update.
// A nil pointer means "leave unchanged"; a non-nil pointer applies the new value.
// For Workflow: nil = unchanged; non-nil = overwrite (even if pointed-to value is nil).
type Update struct {
	Title        *string
	Slug         *string
	Status       *Status
	StatusReason *string
	AgentMode    *string
	TaskType     *TaskType
	Body         *string
	Tags         *[]string
	ProjectID    *string
	Branch       *string
	PRNumber     *int
	Issue        *string
	Reviewed     *bool
	RunRole      *string
	TodoistID    *string
	Workflow     **workflow.Execution
	Plan         *string
	PlanCritique *string
}

// Ptr returns a pointer to v. Convenience for building Update literals.
func Ptr[T any](v T) *T {
	p := new(T)
	*p = v
	return p
}

// UpdateFromMap converts a map[string]any to a typed Update.
// Returns an error if any key is unknown or the value has the wrong type.
// This is the boundary adapter for CLI and Wails callers that receive raw maps.
func UpdateFromMap(raw map[string]any) (Update, error) {
	var u Update
	for k, v := range raw {
		if err := applyMapField(&u, k, v); err != nil {
			return Update{}, err
		}
	}
	return u, nil
}

func applyMapField(u *Update, k string, v any) error {
	switch k {
	case "title", "slug", "status_reason", "agent_mode", "body",
		"project_id", "branch", "issue", "run_role", "todoist_id", "plan", "plan_critique":
		return applyPlainStringField(u, k, v)
	case "status":
		return applyStatusField(u, k, v)
	case "task_type":
		return applyTaskTypeField(u, k, v)
	case "tags":
		return applyTagsField(u, k, v)
	case "pr_number":
		return applyPRNumberField(u, k, v)
	case "reviewed":
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("field %q: want bool, got %T", k, v)
		}
		u.Reviewed = &b
	case "workflow":
		wf, ok := v.(*workflow.Execution)
		if !ok {
			return fmt.Errorf("field %q: want *workflow.Execution, got %T", k, v)
		}
		u.Workflow = &wf
	default:
		return fmt.Errorf("unknown task field %q", k)
	}
	return nil
}

func applyPlainStringField(u *Update, k string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field %q: want string, got %T", k, v)
	}
	switch k {
	case "title":
		u.Title = &s
	case "slug":
		u.Slug = &s
	case "status_reason":
		u.StatusReason = &s
	case "agent_mode":
		u.AgentMode = &s
	case "body":
		u.Body = &s
	case "project_id":
		u.ProjectID = &s
	case "branch":
		u.Branch = &s
	case "issue":
		u.Issue = &s
	case "run_role":
		u.RunRole = &s
	case "todoist_id":
		u.TodoistID = &s
	case "plan":
		u.Plan = &s
	case "plan_critique":
		u.PlanCritique = &s
	}
	return nil
}

func applyStatusField(u *Update, k string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field %q: want string, got %T", k, v)
	}
	st, err := ValidateStatus(s)
	if err != nil {
		return err
	}
	u.Status = &st
	return nil
}

func applyTaskTypeField(u *Update, k string, v any) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("field %q: want string, got %T", k, v)
	}
	tt, err := ValidateTaskType(s)
	if err != nil {
		return err
	}
	u.TaskType = &tt
	return nil
}

func applyTagsField(u *Update, k string, v any) error {
	switch tv := v.(type) {
	case []string:
		cp := make([]string, len(tv))
		copy(cp, tv)
		u.Tags = &cp
	case string:
		parts := strings.Split(tv, ",")
		u.Tags = &parts
	default:
		return fmt.Errorf("field %q: want []string or string, got %T", k, v)
	}
	return nil
}

func applyPRNumberField(u *Update, k string, v any) error {
	switch n := v.(type) {
	case int:
		u.PRNumber = &n
	case float64:
		i := int(n)
		u.PRNumber = &i
	default:
		return fmt.Errorf("field %q: want int or float64, got %T", k, v)
	}
	return nil
}
