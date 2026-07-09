package watchdog

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func TestHasBlocker(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", false},
		{"no blocker", "## Description\nDo the thing.", false},
		{"h2 blocked by", "## Blocked by\n\nhttps://github.com/org/repo/issues/42", true},
		{"h2 blocked by lowercase", "## blocked by\n\nsome text", true},
		{"inline blocked by standalone line", "Blocked by #123\nsome other text", true},
		{"inline blocked by prose — not a blocker", "depends on Blocked by #123", false},
		{"partial match no hash", "## Description\nblocked by upstream change", false},
		{"mid-body heading", "## Summary\nsome work\n\n## Blocked by\n\n#99", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasBlocker(tc.body); got != tc.want {
				t.Fatalf("hasBlocker(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestCheckDwell_SkipsBlockedTasks(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)

	blockedTask, err := mgr.Create("blocked task", "## Blocked by\n\nhttps://github.com/org/repo/issues/1", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Blocker exemption only matters for in-progress tasks now that todo is
	// out of scope for dwell entirely.
	blockedTask, err = mgr.Update(blockedTask.ID, task.Update{
		Status: task.Ptr(task.StatusInProgress),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	w := &Watchdog{
		tasks:  mgr,
		logger: slog.New(slog.DiscardHandler),
	}

	// Run checkDwell with a "now" that is far ahead so any non-blocked task would fire.
	w.checkDwell(blockedTask.UpdatedAt.Add(48 * time.Hour))

	got, err := mgr.Get(blockedTask.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("blocked task status = %q, want in-progress (should be skipped by dwell)", got.Status)
	}
}

func TestCheckDwell_SkipsTodoTask(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)

	tk, err := mgr.Create("normal task", "## Description\nsome work", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = mgr.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusTodo)})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	w := &Watchdog{
		tasks:  mgr,
		logger: slog.New(slog.DiscardHandler),
	}

	// checkDwell with "now" 48h ahead so the default 12h budget would be
	// exceeded if todo were still in scope — it must not be escalated.
	w.checkDwell(tk.UpdatedAt.Add(48 * time.Hour))

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusTodo {
		t.Fatalf("status = %q, want todo (dwell must not escalate todo tasks)", got.Status)
	}
}

func TestCheckDwell_SkipsTaskWithRunningAgent(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)

	tk, err := mgr.Create("normal task", "## Description\nsome work", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = mgr.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInProgress)})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	w := &Watchdog{
		tasks:           mgr,
		logger:          slog.New(slog.DiscardHandler),
		hasRunningAgent: func(taskID string) bool { return taskID == tk.ID },
	}

	// checkDwell with "now" 48h ahead so the default 12h budget would be
	// exceeded, but a live running agent must suppress the escalation — only
	// the task file is stale, not the agent doing the work.
	w.checkDwell(tk.UpdatedAt.Add(48 * time.Hour))

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (running agent should suppress dwell escalation)", got.Status)
	}
}

func TestCheckDwell_EscalatesUnblockedInProgressTask(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)

	tk, err := mgr.Create("normal task", "## Description\nsome work", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = mgr.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInProgress)})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	w := &Watchdog{
		tasks:  mgr,
		logger: slog.New(slog.DiscardHandler),
	}

	// checkDwell with "now" 48h ahead so the default 12h budget is exceeded.
	w.checkDwell(tk.UpdatedAt.Add(48 * time.Hour))

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason == "dwell exceeded size tag budget" {
		t.Fatalf("status reason still has hardcoded string; want budget-specific reason")
	}
}
