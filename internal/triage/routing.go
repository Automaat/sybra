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

// RouteMode returns the mode to persist for a triaged task. The LLM's pick is
// currently passed through unchanged; headless is the preferred execution mode
// (see the interactive-removal umbrella), so no project type is forced to
// interactive.
func RouteMode(llmMode, taskType, projectType string) string {
	_ = taskType
	_ = projectType
	return llmMode
}
