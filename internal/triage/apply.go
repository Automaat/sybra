package triage

import (
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// Apply writes the classifier verdict to the task via Manager.UpdateMap.
// All field changes happen in a single UpdateMap call so the write is
// atomic per task (Manager holds a per-task mutex).
//
// projects is used to look up the project type after ProjectID is matched,
// which feeds into routing rules (work projects force interactive mode).
func Apply(mgr *task.Manager, t task.Task, v Verdict, projects []project.Project) (task.Task, error) {
	updates := make(map[string]any, 8)

	projectID := v.ProjectID
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
		updates["tags"] = v.Tags
	}

	mode := RouteMode(v.Mode, v.Type, projectType)
	updates["agent_mode"] = mode

	if projectID != "" {
		updates["project_id"] = projectID
	}

	status := RouteStatus(v.Size, v.Type, projectType)
	updates["status"] = string(status)
	updates["status_reason"] = "triage"

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
