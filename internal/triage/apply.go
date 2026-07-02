package triage

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

// preservedTags are tags never emitted by the classifier vocabulary but that
// must survive a wholesale tag-replacement in Apply because dropping them
// would silently break routing the task depends on: escape-hatch opt-outs
// (see escapeHatchTags) and the umbrella dependency gate marker.
var preservedTags = append(append([]string{}, escapeHatchTags...), umbrella.GatedTag)

// Apply writes the classifier verdict to the task via Manager.UpdateMap.
// All field changes happen in a single UpdateMap call so the write is
// atomic per task (Manager holds a per-task mutex).
//
// projects is used to look up the project type after ProjectID is matched,
// which feeds into routing rules (work projects force interactive mode).
func Apply(mgr *task.Manager, t task.Task, v Verdict, projects []project.Project) (task.Task, error) {
	updates := make(map[string]any, 8)

	projectID := strings.TrimSpace(v.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(t.ProjectID)
	}
	if projectID == "" {
		projectID = MatchProject(t.Title, t.Body, projects)
	}

	projectType := ""
	if projectID != "" {
		for i := range projects {
			if projects[i].ID == projectID {
				projectType = string(projects[i].Type)
				break
			}
		}
	}

	newTitle := strings.TrimSpace(v.Title)
	if newTitle == "" {
		return task.Task{}, fmt.Errorf("empty verdict title")
	}
	if newTitle != t.Title {
		updates["title"] = newTitle
		updates["body"] = prependOriginalTitle(t.Body, t.Title)
	}

	if strings.TrimSpace(t.Body) == "" && v.Description != "" {
		body := v.Description
		if existing, ok := updates["body"].(string); ok {
			body = existing + "\n\n" + v.Description
		}
		updates["body"] = body
	}

	if len(v.Tags) > 0 {
		// Preserve routing tags the classifier vocabulary excludes
		// (escape-hatch opt-outs, umbrella gate marker) — replacing tags
		// wholesale would otherwise silently drop them.
		tags := v.Tags
		for _, keep := range preservedTags {
			if slices.Contains(t.Tags, keep) && !slices.Contains(tags, keep) {
				tags = append(tags, keep)
			}
		}
		updates["tags"] = tags
	}

	mode := RouteMode(v.Mode, v.Type, projectType)
	updates["agent_mode"] = mode

	if projectID != "" {
		updates["project_id"] = projectID
	}

	status := RouteStatus(v.Size, v.Type, projectType)
	// pr-fix tasks (system-created to fix an existing PR) must never enter the
	// planning phase — they go straight to implementation. Override any route
	// that would park them in planning.
	if t.RunRole == "pr-fix" || t.PRNumber > 0 {
		status = task.StatusTodo
	}
	updates["status"] = string(status)
	// No status_reason on successful triage: the field is reserved for
	// attention-worthy states (monitor/watchdog/blocked), which the UI renders
	// as a warning. Setting "status" without a reason makes the store clear any
	// stale reason (e.g. "monitor: awaiting triage").

	updated, err := mgr.UpdateMap(t.ID, updates)
	if err != nil {
		return task.Task{}, fmt.Errorf("update task: %w", err)
	}
	return updated, nil
}

// prependOriginalTitle adds a line preserving the original verbose title
// above any existing body content. Idempotent: if the body already contains
// the original-title marker, it is returned unchanged.
func prependOriginalTitle(body, originalTitle string) string {
	marker := "**Original title:** "
	if strings.Contains(body, marker) {
		return body
	}
	line := marker + originalTitle
	if strings.TrimSpace(body) == "" {
		return line
	}
	return line + "\n\n" + body
}
