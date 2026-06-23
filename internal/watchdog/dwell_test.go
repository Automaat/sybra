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
	// Push UpdatedAt far into the past to exceed any budget.
	past := time.Now().UTC().Add(-24 * time.Hour)
	blockedTask, err = mgr.Update(blockedTask.ID, task.Update{
		Status: task.Ptr(task.StatusTodo),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	// Manually set UpdatedAt via a second Update with a body touch to keep past time.
	// We can't set UpdatedAt directly — use a workaround: set the task's body again so
	// the store re-marshals. Then verify the watchdog skips it regardless.
	_ = past // UpdatedAt is set by the store on each Update call.
	// Use a real old task by setting updatedAt via internal store if needed.
	// Since we cannot set UpdatedAt, we verify the skip via status after checkDwell.

	w := &Watchdog{
		tasks:  mgr,
		logger: slog.New(slog.DiscardHandler),
	}

	// Run checkDwell with a "now" that is far ahead so any non-blocked task would fire.
	w.checkDwell(time.Now().Add(48 * time.Hour))

	got, err := mgr.Get(blockedTask.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusTodo {
		t.Fatalf("blocked task status = %q, want todo (should be skipped by dwell)", got.Status)
	}
}

func TestCheckDwell_EscalatesUnblockedTask(t *testing.T) {
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
