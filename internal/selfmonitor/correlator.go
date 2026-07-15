package selfmonitor

import (
	"fmt"
	"sort"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/task"
)

// Correlate groups a slice of InvestigatedFinding into cross-finding
// Correlation entries using three deterministic strategies:
//
//   - same_project  — ≥2 findings whose TaskID resolves to the same ProjectID
//   - same_error_class — ≥2 findings whose LogSummary top error class matches
//   - cascade — agent_retry_loop + stuck_task sharing the same TaskID
//
// getTask is used only for same_project lookups; a nil result or error is
// treated as "no project" and the finding is skipped for that strategy.
// Returns nil when fewer than 2 findings are supplied.
func Correlate(findings []InvestigatedFinding, getTask func(id string) (task.Task, error)) []Correlation {
	if len(findings) < 2 {
		return nil
	}

	var out []Correlation
	out = append(out, correlateByProject(findings, getTask)...)
	out = append(out, correlateByErrorClass(findings)...)
	out = append(out, correlateCascades(findings)...)
	return out
}

func correlateByProject(findings []InvestigatedFinding, getTask func(id string) (task.Task, error)) []Correlation {
	if getTask == nil {
		return nil
	}
	byProject := map[string][]string{} // projectID → fingerprints
	for i := range findings {
		inv := &findings[i]
		if inv.Finding.TaskID == "" {
			continue
		}
		t, err := getTask(inv.Finding.TaskID)
		if err != nil || t.ProjectID == "" {
			continue
		}
		byProject[t.ProjectID] = append(byProject[t.ProjectID], inv.Fingerprint)
	}
	var out []Correlation
	projectIDs := make([]string, 0, len(byProject))
	for projID := range byProject {
		projectIDs = append(projectIDs, projID)
	}
	sort.Strings(projectIDs)
	for _, projID := range projectIDs {
		fps := append([]string(nil), byProject[projID]...)
		if len(fps) < 2 {
			continue
		}
		sort.Strings(fps)
		out = append(out, Correlation{
			Kind:         "same_project",
			Key:          projID,
			Count:        len(fps),
			Fingerprints: fps,
			Description:  fmt.Sprintf("%d findings share project %s", len(fps), projID),
		})
	}
	return out
}

func correlateByErrorClass(findings []InvestigatedFinding) []Correlation {
	byClass := map[string][]string{} // error class → fingerprints
	for i := range findings {
		inv := &findings[i]
		if inv.LogSummary == nil || len(inv.LogSummary.ErrorClasses) == 0 {
			continue
		}
		// ErrorClasses sorted by count descending; use the dominant class.
		cls := inv.LogSummary.ErrorClasses[0].Class
		if cls == "" || cls == "tool_error" || cls == "unknown" {
			continue
		}
		byClass[cls] = append(byClass[cls], inv.Fingerprint)
	}
	var out []Correlation
	classes := make([]string, 0, len(byClass))
	for cls := range byClass {
		classes = append(classes, cls)
	}
	sort.Strings(classes)
	for _, cls := range classes {
		fps := append([]string(nil), byClass[cls]...)
		if len(fps) < 2 {
			continue
		}
		sort.Strings(fps)
		out = append(out, Correlation{
			Kind:         "same_error_class",
			Key:          cls,
			Count:        len(fps),
			Fingerprints: fps,
			Description:  fmt.Sprintf("%d findings share error class %q", len(fps), cls),
		})
	}
	return out
}

func correlateCascades(findings []InvestigatedFinding) []Correlation {
	retryByTask := map[string]string{} // taskID → fingerprint
	stuckByTask := map[string]string{}
	for i := range findings {
		inv := &findings[i]
		tid := inv.Finding.TaskID
		if tid == "" {
			continue
		}
		switch inv.Finding.Category {
		case health.CatAgentRetryLoop:
			retryByTask[tid] = inv.Fingerprint
		case health.CatStuckTask:
			stuckByTask[tid] = inv.Fingerprint
		default:
		}
	}
	var out []Correlation
	taskIDs := make([]string, 0, len(retryByTask))
	for tid := range retryByTask {
		taskIDs = append(taskIDs, tid)
	}
	sort.Strings(taskIDs)
	for _, tid := range taskIDs {
		fp1 := retryByTask[tid]
		fp2, ok := stuckByTask[tid]
		if !ok {
			continue
		}
		fps := []string{fp1, fp2}
		sort.Strings(fps)
		out = append(out, Correlation{
			Kind:         "cascade",
			Key:          tid,
			Count:        2,
			Fingerprints: fps,
			Description:  fmt.Sprintf("agent_retry_loop → stuck_task cascade on task %s", tid),
		})
	}
	return out
}
