package triage

import "github.com/Automaat/sybra/internal/task"

// RouteStatus picks the next task status from the classifier verdict.
//
// Rules (in order of precedence):
//   - review           → todo
//   - work project     → planning
//   - medium/large feature → planning
//   - everything else  → todo
//
// projectType is the owning project.ProjectType (string) if the task is
// project-linked, or "" if unlinked.
func RouteStatus(size, taskType, projectType string) task.Status {
	if taskType == "review" {
		return task.StatusTodo
	}
	if projectType == "work" {
		return task.StatusPlanning
	}
	if taskType == "feature" && (size == "medium" || size == "large") {
		return task.StatusPlanning
	}
	return task.StatusTodo
}

// RouteMode returns the mode to persist for a triaged task. The interactive
// runner was removed, so headless is the only dispatchable mode regardless of
// what the LLM emits — this is a deterministic floor, not a pass-through:
// the schema enum already restricts llmMode to "headless", but a model can
// still hallucinate outside its schema, and this must not silently mint an
// undispatchable task. taskType/projectType are retained in the signature for
// callers and future routing.
func RouteMode(llmMode, taskType, projectType string) string {
	return task.AgentModeHeadless
}
