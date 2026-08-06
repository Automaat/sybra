// Package taskstatus owns the task status vocabulary.
//
// It lives apart from internal/task because internal/task imports
// internal/workflow, so the engine cannot reach task.Status without a cycle —
// and the engine is where most status comparisons happen. Left in
// internal/task, the vocabulary was re-typed as bare string literals across
// that boundary, where a typo is a silently dead branch rather than a build
// failure. Depending on nothing keeps this importable from either side.
package taskstatus

import "fmt"

// Status is a task's position in the pipeline. Values are persisted verbatim
// in task frontmatter, so a constant's spelling is a wire format: changing one
// orphans every task file already on disk. Callers should parse untrusted
// input with Validate rather than casting a string.
type Status string

const (
	New           Status = "new"
	Todo          Status = "todo"
	InProgress    Status = "in-progress"
	ReadyReview   Status = "ready-review"
	InReview      Status = "in-review"
	Planning      Status = "planning"
	PlanReview    Status = "plan-review"
	Testing       Status = "testing"
	ReadyPR       Status = "ready-pr"
	HumanRequired Status = "human-required"
	Blocked       Status = "blocked"
	Done          Status = "done"
	Cancelled     Status = "cancelled"
)

var valid = map[Status]bool{
	New: true, Todo: true, InProgress: true,
	ReadyReview: true, InReview: true,
	Planning: true, PlanReview: true,
	Testing: true, ReadyPR: true,
	HumanRequired: true, Blocked: true,
	Done: true, Cancelled: true,
}

// All returns every valid status in display order.
func All() []Status {
	return []Status{
		New, Todo, Planning, PlanReview,
		InProgress, ReadyReview, InReview,
		Testing, ReadyPR,
		HumanRequired, Blocked, Done, Cancelled,
	}
}

// IsTerminal reports whether s is a terminal (closed) status.
func IsTerminal(s Status) bool {
	return s == Done || s == Cancelled
}

// Validate parses s into a Status, returning an error naming every valid
// status if s does not match one of the known constants.
func Validate(s string) (Status, error) {
	st := Status(s)
	if !valid[st] {
		return "", fmt.Errorf("invalid status %q (valid: %v)", s, All())
	}
	return st, nil
}
