package workflow

import (
	"fmt"
	"strings"
)

// EvalCondition evaluates a condition against a context map of field→value.
// Fields use dot notation: "task.status", "task.tags", "vars.human_action".
func EvalCondition(c Condition, fields map[string]string) bool {
	val, exists := fields[c.Field]

	switch c.Operator {
	case "exists":
		return exists
	case "equals":
		return val == c.Value
	case "not_equals":
		return val != c.Value
	case "contains":
		return strings.Contains(val, c.Value)
	case "not_contains":
		return !strings.Contains(val, c.Value)
	default:
		return false
	}
}

// EvalConditions returns true if ALL conditions match (AND logic).
func EvalConditions(conditions []Condition, fields map[string]string) bool {
	for _, c := range conditions {
		if !EvalCondition(c, fields) {
			return false
		}
	}
	return true
}

// ResolveTransition evaluates a step's transitions and returns the target step ID.
// Returns "" if no transition matches (end of workflow) or the step has no transitions.
func ResolveTransition(transitions []Transition, fields map[string]string) (string, error) {
	for _, t := range transitions {
		if t.When == nil {
			return t.GoTo, nil // default/fallback transition
		}
		if EvalCondition(*t.When, fields) {
			return t.GoTo, nil
		}
	}
	if len(transitions) == 0 {
		return "", nil
	}
	return "", fmt.Errorf("no matching transition found")
}
