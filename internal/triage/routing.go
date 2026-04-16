package triage

import "github.com/Automaat/sybra/internal/task"

// RouteStatus picks the next task status from the classifier verdict.
//
// Rules (in order of precedence):
//   - review              → todo
//   - medium/large feature → planning (work project always planning)
//   - everything else     → todo
//
// projectType is the owning project.ProjectType (string) if the task is
// project-linked, or "" if unlinked. Currently "work" projects only tighten
// the planning rule (they are already covered by the feature rule above),
// but the argument is kept so apply.go can tweak mode for work projects.
func RouteStatus(size, taskType, projectType string) task.Status {
	_ = projectType // reserved for future rules
	if taskType == "review" {
		return task.StatusTodo
	}
	if taskType == "feature" && (size == "medium" || size == "large") {
		return task.StatusPlanning
	}
	return task.StatusTodo
}

// RouteMode optionally overrides the LLM's mode pick based on project type.
// Work projects get forced to interactive unless the task is a PR review.
func RouteMode(llmMode, taskType, projectType string) string {
	if projectType == "work" && taskType != "review" {
		return task.AgentModeInteractive
	}
	return llmMode
}
