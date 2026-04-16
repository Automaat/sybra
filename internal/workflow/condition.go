package workflow

import (
	"fmt"
	"strings"
)

// KnownTriggerFields is the authoritative set of field names that engine
// callers populate for trigger and transition evaluation. Workflow YAML must
// reference only these keys (plus the "vars." prefix for step-local variables
// seeded by DispatchEvent/StartWorkflowWithVars callers).
//
// Keep this in lock-step with taskFields() in engine.go and with the extras
// that DispatchEvent callers inject (e.g. "pr.issue_kind" from app_reviews
// and svc_integrations). A field referenced in YAML but missing from here
// will silently never match — exactly the class of bug that left auto-merge
// dead code, since its "project.type" condition had no populating caller.
var KnownTriggerFields = map[string]bool{
	// Populated by engine.taskFields for every dispatch/transition.
	"task.id":         true,
	"task.title":      true,
	"task.status":     true,
	"task.tags":       true,
	"task.agent_mode": true,
	"task.project_id": true,
	"task.branch":     true,
	"task.pr_number":  true,
	"task.reviewed":   true,
	// Supplied as extras by DispatchEvent("pr.event", ...) callers in
	// app_reviews.go and svc_integrations.go.
	"pr.issue_kind": true,
}

// isKnownField reports whether a YAML trigger/transition field name refers
// to a key the engine will actually populate. The "vars." prefix is open-
// ended because step variables are seeded dynamically by callers.
func isKnownField(field string) bool {
	if field == "" {
		return true
	}
	if strings.HasPrefix(field, "vars.") {
		return true
	}
	return KnownTriggerFields[field]
}

// FieldAllowedValues maps enum-shaped trigger fields to the finite set of
// values the engine may actually see at runtime. Workflow YAML conditions
// that compare against an enum must pick a value from this set, otherwise
// the condition is dead on arrival (the classic ci-failure vs ci_failure
// shape of bug).
//
// Only enum-shaped scalar fields belong here. Free-form fields (task.id,
// task.title, task.tags, task.branch, task.project_id) and vars.* have no
// registered set and skip enum validation.
//
// Keep this in lock-step with:
//   - task.AllStatuses() in internal/task/model.go  → "task.status"
//   - task.AllTaskTypes()                           → "task.task_type"
//   - github.PRIssueKind constants in internal/github/monitor.go
//     → "pr.issue_kind"
//
// The main-package test TestWorkflowFieldAllowedValues_MatchEnumSources
// cross-checks this map against those enum sources at build time so any
// drift (adding a new Status, renaming a kind) fails CI immediately.
var FieldAllowedValues = map[string]map[string]bool{
	"task.status": {
		"new": true, "todo": true, "in-progress": true, "in-review": true,
		"planning": true, "plan-review": true, "testing": true,
		"test-plan-review": true, "human-required": true, "done": true,
		"cancelled": true,
	},
	"task.task_type": {
		"normal": true, "debug": true, "research": true, "chat": true,
	},
	"pr.issue_kind": {
		"conflict": true, "ci_failure": true, "ready_to_merge": true,
	},
}

// checkEnumValue returns the unknown values (if any) for a condition that
// targets an enum-shaped field. For the csv-style operators (in/not_in)
// the comparison value is split and each entry checked independently.
// The `exists` operator skips enum validation — it only tests field
// presence, so the value is irrelevant.
func checkEnumValue(field, operator, value string) []string {
	allowed, ok := FieldAllowedValues[field]
	if !ok {
		return nil
	}
	switch operator {
	case "equals", "not_equals":
		if value != "" && !allowed[value] {
			return []string{value}
		}
	case "in", "not_in":
		var bad []string
		for v := range strings.SplitSeq(value, ",") {
			trimmed := strings.TrimSpace(v)
			if trimmed == "" {
				continue
			}
			if !allowed[trimmed] {
				bad = append(bad, trimmed)
			}
		}
		return bad
	}
	return nil
}

// checkEnumOperator reports whether the operator is semantically wrong for
// an enum-shaped field. `contains`/`not_contains` do substring matching,
// which silently fails on scalar enums (e.g. `contains "conflict,ci_failure"`
// never matches `"ci_failure"`). Only free-form fields like task.tags — which
// are joined into a single comma-separated string — should use these.
func checkEnumOperator(field, operator string) bool {
	if _, ok := FieldAllowedValues[field]; !ok {
		return false
	}
	return operator == "contains" || operator == "not_contains"
}

// EvalCondition evaluates a condition against a context map of field→value.
// Fields use dot notation: "task.status", "task.tags", "vars.human_action".
//
// Operators:
//   - exists / equals / not_equals: single-value checks
//   - contains / not_contains: substring check — field value must contain
//     the condition value. Commonly used for comma-joined list fields
//     (e.g. task.tags) to test element presence.
//   - in / not_in: membership check — condition value is a comma-separated
//     list of allowed values, field must match one of them. Use this when
//     the field is a single scalar (e.g. pr.issue_kind) and the trigger
//     should accept several alternatives.
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
	case "in":
		return inList(val, c.Value)
	case "not_in":
		return !inList(val, c.Value)
	default:
		return false
	}
}

// inList reports whether val exactly matches one of the comma-separated
// entries in csv (whitespace trimmed around each entry).
func inList(val, csv string) bool {
	for v := range strings.SplitSeq(csv, ",") {
		if strings.TrimSpace(v) == val {
			return true
		}
	}
	return false
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
