package watchdog

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

const (
	DwellTickInterval = 5 * time.Minute
	dwellSmall        = 90 * time.Minute
	dwellMedium       = 6 * time.Hour
	dwellLarge        = 18 * time.Hour
	dwellDefault      = 12 * time.Hour
)

// dwellBudget returns how long a task may stay in an actionable status before
// being escalated to human-required. Only applies to todo and in-progress.
func dwellBudget(tags []string) time.Duration {
	switch {
	case slices.Contains(tags, "large"):
		return dwellLarge
	case slices.Contains(tags, "small"):
		return dwellSmall
	case slices.Contains(tags, "medium"):
		return dwellMedium
	default:
		return dwellDefault
	}
}

// hasBlocker reports whether the task body declares an upstream blocker via a
// "## Blocked by" heading or a standalone "Blocked by #NNN" line. Tasks with
// a recognised blocker are skipped by the dwell watchdog — they are
// intentionally idle and should not be escalated until the blocker clears.
//
// Matching is line-anchored to avoid false positives from prose that contains
// the phrase mid-sentence (e.g. "it depends on Blocked by #123").
func hasBlocker(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(strings.ToLower(line))
		if strings.HasPrefix(trimmed, "## blocked by") || strings.HasPrefix(trimmed, "blocked by #") {
			return true
		}
	}
	return false
}

func (w *Watchdog) checkDwell(now time.Time) {
	tasks, err := w.tasks.List()
	if err != nil {
		w.logger.Warn("watchdog.dwell.list", "err", err)
		return
	}
	for i := range tasks {
		t := &tasks[i]
		if t.TaskType == task.TaskTypeChat {
			continue
		}
		if t.Status != task.StatusTodo && t.Status != task.StatusInProgress {
			continue
		}
		if hasBlocker(t.Body) {
			continue
		}
		budget := dwellBudget(t.Tags)
		if now.Sub(t.UpdatedAt) <= budget {
			continue
		}
		reason := fmt.Sprintf("dwell: exceeded %v budget", budget)
		w.logger.Info("watchdog.dwell.escalate",
			"task_id", t.ID, "status", string(t.Status),
			"dwell_h", now.Sub(t.UpdatedAt).Hours(), "budget_h", budget.Hours())
		if _, err := w.tasks.Update(t.ID, task.Update{
			Status:       task.Ptr(task.StatusHumanRequired),
			StatusReason: task.Ptr(reason),
		}); err != nil {
			w.logger.Error("watchdog.dwell.update", "task_id", t.ID, "err", err)
		}
	}
}
