package watchdog

import (
	"fmt"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// checkRunRate escalates an in-progress task whose dispatch loop is
// thrashing — many agent runs of the same role landing within a short
// trailing window — rather than waiting for the dwell budget (hours) to
// notice. See #2134: a task accumulated 1204 implementation runs over ~29h,
// each ~18-90s apart and all costUsd:0/instant-stopped, before dwell finally
// escalated it. MaxRunsPerWindow <= 0 disables the check.
func (w *Watchdog) checkRunRate(now time.Time) {
	if w.maxRunsPerWindow <= 0 || w.runWindow <= 0 {
		return
	}
	tasks, err := w.tasks.List()
	if err != nil {
		w.logger.Warn("watchdog.runrate.list", "err", err)
		return
	}
	for i := range tasks {
		t := &tasks[i]
		if t.TaskType == task.TaskTypeChat {
			continue
		}
		if t.Status != task.StatusInProgress {
			continue
		}
		// Same exemption as checkDwell: a live headless agent may itself be
		// producing the burst while genuinely recovering, and escalating
		// here would get it force-stopped on the very next tick.
		if w.hasLiveHeadlessAgent != nil && w.hasLiveHeadlessAgent(t.ID) {
			continue
		}
		role, count := recentRunBurst(t.AgentRuns, now, w.runWindow)
		if count < w.maxRunsPerWindow {
			continue
		}
		reason := fmt.Sprintf("run_rate: %d %q runs within %v — dispatch loop suspected", count, role, w.runWindow)
		w.logger.Info("watchdog.runrate.escalate",
			"task_id", t.ID, "role", role, "count", count, "window_m", w.runWindow.Minutes())
		if _, err := w.tasks.Update(t.ID, task.Update{
			Status:       task.Ptr(task.StatusHumanRequired),
			StatusReason: task.Ptr(reason),
		}); err != nil {
			w.logger.Error("watchdog.runrate.update", "task_id", t.ID, "err", err)
		}
	}
}

// recentRunBurst returns the role with the most agent runs started within
// the trailing window ending at now, and that role's count. Runs are always
// appended chronologically (internal/task.Store.AddRunWithStatus), so a
// task with thousands of historical runs is scanned only back to the window
// boundary, not in full, once the burst itself has ended.
func recentRunBurst(runs []task.AgentRun, now time.Time, window time.Duration) (role string, count int) {
	cutoff := now.Add(-window)
	counts := make(map[string]int)
	for i := range slices.Backward(runs) {
		if runs[i].StartedAt.Before(cutoff) {
			break
		}
		// Legacy runs recorded Role as "" for implementation (see
		// internal/stats/store.go's identical normalization); without this
		// a burst split across old and new records would undercount on
		// both buckets and never trip.
		r := runs[i].Role
		if r == "" {
			r = "implementation"
		}
		counts[r]++
	}
	for r, c := range counts {
		if c > count {
			role, count = r, c
		}
	}
	return role, count
}
