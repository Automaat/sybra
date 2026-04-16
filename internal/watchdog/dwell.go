package watchdog

import (
	"slices"
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
		budget := dwellBudget(t.Tags)
		if now.Sub(t.UpdatedAt) <= budget {
			continue
		}
		reason := "dwell exceeded size tag budget"
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
