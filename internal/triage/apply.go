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

	// t.ProjectID is sticky: once set (e.g. by the GitHub issue fetcher at
	// task creation), the classifier's free-text guess must never override
	// it with a lower-confidence, content-similarity match. But a sticky ID
	// is only authoritative while it still resolves to a registered project —
	// a renamed/deleted project leaves a stale ID that would otherwise lock
	// the task to an empty project type, silently skipping the work-typed
	// forced-interactive/forced-planning routing. So keep it only when it
	// resolves; otherwise re-resolve, preferring the task's own Issue URL —
	// the authoritative source-of-truth link — over the classifier's guess
	// and generic title/body scanning.
	projectID := strings.TrimSpace(t.ProjectID)
	projectType, resolved := projectTypeFor(projectID, projects)
	// A non-empty t.ProjectID that fails to resolve is stale (renamed/deleted
	// project) and must be explicitly cleared below if re-resolution also
	// comes up empty — otherwise the task stays stuck on an unresolvable id.
	stale := projectID != "" && !resolved
	if !resolved {
		projectID = MatchProjectFromIssue(t.Issue, projects)
		if projectID == "" {
			guess := strings.TrimSpace(v.ProjectID)
			if _, ok := projectTypeFor(guess, projects); ok {
				projectID = guess
			}
		}
		if projectID == "" {
			projectID = MatchProject(t.Title, t.Body, projects)
		}
		projectType, _ = projectTypeFor(projectID, projects)
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
		tags := slices.Clone(v.Tags)
		for _, keep := range preservedTags {
			if slices.Contains(t.Tags, keep) && !slices.Contains(tags, keep) {
				tags = append(tags, keep)
			}
		}
		updates["tags"] = tags
	}

	mode := RouteMode(v.Mode, v.Type, projectType)
	updates["agent_mode"] = mode

	if projectID != "" || stale {
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

	// A ☂️-titled task that never went through the GitHub issue fetcher (e.g.
	// manual sybra-cli create) keeps task_type=normal and is invisible to the
	// umbrella gate (internal/sybra/app_umbrella_gate.go), which filters
	// strictly on TaskTypeUmbrella. Dispatching it as a flat implement task
	// wastes a full run before the agent discovers there's no direct code
	// surface. Catch it here — before dispatch — and park it for a human to
	// either set task_type=umbrella or fix the title.
	effectiveTitle := newTitle
	if effectiveTitle == "" {
		effectiveTitle = t.Title
	}
	if t.TaskType != task.TaskTypeUmbrella && umbrella.IsUmbrellaIssue(effectiveTitle, t.Tags) {
		updates["status"] = string(task.StatusHumanRequired)
		updates["status_reason"] = "☂️-titled task has task_type=normal, not umbrella — " +
			"guard blocked dispatch to avoid a wasted implement run; " +
			"set task_type=umbrella to expand it or fix the title if this isn't a tracker"
	}

	updated, err := mgr.UpdateMap(t.ID, updates)
	if err != nil {
		return task.Task{}, fmt.Errorf("update task: %w", err)
	}
	return updated, nil
}

// projectTypeFor returns the registered project type for id and whether id
// resolves to a registered project. An empty id never resolves.
func projectTypeFor(id string, projects []project.Project) (string, bool) {
	if id == "" {
		return "", false
	}
	for i := range projects {
		if projects[i].ID == id {
			return string(projects[i].Type), true
		}
	}
	return "", false
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
