package watchdog

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func newInProgressTask(t *testing.T, mgr *task.Manager) task.Task {
	t.Helper()
	tk, err := mgr.Create("burst task", "## Description\nsome work", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = mgr.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInProgress)})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	return tk
}

func addRuns(t *testing.T, mgr *task.Manager, taskID, role string, n int, start time.Time, spacing time.Duration) {
	t.Helper()
	for i := range n {
		if err := mgr.AddRun(taskID, task.AgentRun{
			AgentID:   "agent",
			Role:      role,
			StartedAt: start.Add(time.Duration(i) * spacing),
		}); err != nil {
			t.Fatalf("add run: %v", err)
		}
	}
}

func TestCheckRunRate_EscalatesBurstOfSameRole(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)
	tk := newInProgressTask(t, mgr)

	now := time.Now()
	// 30 implementation runs, 18s apart, all inside the trailing 30m window.
	addRuns(t, mgr, tk.ID, "implementation", 30, now.Add(-29*time.Minute), 18*time.Second)

	w := &Watchdog{tasks: mgr, logger: slog.New(slog.DiscardHandler), maxRunsPerWindow: 30, runWindow: 30 * time.Minute}
	w.checkRunRate(now)

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if !strings.Contains(got.StatusReason, "run_rate") || !strings.Contains(got.StatusReason, "implementation") {
		t.Fatalf("status reason = %q, want it to mention run_rate and the role", got.StatusReason)
	}
}

func TestCheckRunRate_SkipsBelowThreshold(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)
	tk := newInProgressTask(t, mgr)

	now := time.Now()
	addRuns(t, mgr, tk.ID, "implementation", 29, now.Add(-29*time.Minute), 18*time.Second)

	w := &Watchdog{tasks: mgr, logger: slog.New(slog.DiscardHandler), maxRunsPerWindow: 30, runWindow: 30 * time.Minute}
	w.checkRunRate(now)

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (29 runs is below the 30-run threshold)", got.Status)
	}
}

func TestCheckRunRate_IgnoresRunsOutsideWindow(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)
	tk := newInProgressTask(t, mgr)

	now := time.Now()
	// 40 old runs well outside the window, then only 5 recent ones.
	addRuns(t, mgr, tk.ID, "implementation", 40, now.Add(-5*time.Hour), time.Minute)
	addRuns(t, mgr, tk.ID, "implementation", 5, now.Add(-4*time.Minute), time.Minute)

	w := &Watchdog{tasks: mgr, logger: slog.New(slog.DiscardHandler), maxRunsPerWindow: 30, runWindow: 30 * time.Minute}
	w.checkRunRate(now)

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (old runs outside the window must not count)", got.Status)
	}
}

func TestCheckRunRate_SkipsNonInProgressTask(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)

	tk, err := mgr.Create("todo burst task", "## Description\nsome work", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = mgr.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusTodo)})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	now := time.Now()
	addRuns(t, mgr, tk.ID, "implementation", 30, now.Add(-29*time.Minute), 18*time.Second)

	w := &Watchdog{tasks: mgr, logger: slog.New(slog.DiscardHandler), maxRunsPerWindow: 30, runWindow: 30 * time.Minute}
	w.checkRunRate(now)

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusTodo {
		t.Fatalf("status = %q, want todo (run-rate must only watch in-progress tasks)", got.Status)
	}
}

func TestCheckRunRate_DoesNotSumAcrossRoles(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)
	tk := newInProgressTask(t, mgr)

	now := time.Now()
	// 20 implementation + 20 pr-fix runs inside the window — neither role
	// alone reaches the 30-run threshold, so this must not escalate even
	// though the task has 40 total runs in the window.
	addRuns(t, mgr, tk.ID, "implementation", 20, now.Add(-29*time.Minute), time.Minute)
	addRuns(t, mgr, tk.ID, "pr-fix", 20, now.Add(-29*time.Minute), time.Minute)

	w := &Watchdog{tasks: mgr, logger: slog.New(slog.DiscardHandler), maxRunsPerWindow: 30, runWindow: 30 * time.Minute}
	w.checkRunRate(now)

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (no single role reached the threshold)", got.Status)
	}
}

func TestCheckRunRate_DisabledWhenThresholdIsZero(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)
	tk := newInProgressTask(t, mgr)

	now := time.Now()
	addRuns(t, mgr, tk.ID, "implementation", 50, now.Add(-29*time.Minute), time.Second)

	w := &Watchdog{tasks: mgr, logger: slog.New(slog.DiscardHandler), maxRunsPerWindow: 0, runWindow: 30 * time.Minute}
	w.checkRunRate(now)

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (maxRunsPerWindow<=0 disables the check)", got.Status)
	}
}

func TestCheckRunRate_SkipsTaskWithRunningAgent(t *testing.T) {
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	mgr := task.NewManager(store, nil)
	tk := newInProgressTask(t, mgr)

	now := time.Now()
	addRuns(t, mgr, tk.ID, "implementation", 30, now.Add(-29*time.Minute), 18*time.Second)

	w := &Watchdog{
		tasks:                mgr,
		logger:               slog.New(slog.DiscardHandler),
		maxRunsPerWindow:     30,
		runWindow:            30 * time.Minute,
		hasLiveHeadlessAgent: func(taskID string) bool { return taskID == tk.ID },
	}
	w.checkRunRate(now)

	got, err := mgr.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (running agent should suppress run-rate escalation)", got.Status)
	}
}

func TestRecentRunBurst(t *testing.T) {
	now := time.Now()
	runs := []task.AgentRun{
		{Role: "implementation", StartedAt: now.Add(-2 * time.Hour)}, // outside window
		{Role: "implementation", StartedAt: now.Add(-20 * time.Minute)},
		{Role: "implementation", StartedAt: now.Add(-10 * time.Minute)},
		{Role: "pr-fix", StartedAt: now.Add(-5 * time.Minute)},
	}
	role, count := recentRunBurst(runs, now, 30*time.Minute)
	if role != "implementation" || count != 2 {
		t.Fatalf("recentRunBurst = (%q, %d), want (\"implementation\", 2)", role, count)
	}

	role, count = recentRunBurst(nil, now, 30*time.Minute)
	if role != "" || count != 0 {
		t.Fatalf("recentRunBurst(nil) = (%q, %d), want (\"\", 0)", role, count)
	}
}
