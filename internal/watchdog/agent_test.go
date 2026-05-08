package watchdog

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
)

func TestApplyVerdict_EscalateLeavesTaskRunning(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "ambiguous environment churn",
		Recommendation: "escalate",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want empty", got.StatusReason)
	}
	if stopped {
		t.Fatal("stopAgent called on escalate verdict")
	}
}

func TestApplyVerdict_StopSetsReasonAndStopsAgent(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "looping on toolchain setup",
		Recommendation: "stop",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if got.StatusReason != "watchdog: looping on toolchain setup" {
		t.Fatalf("status_reason = %q, want watchdog reason", got.StatusReason)
	}
	if !stopped {
		t.Fatal("stopAgent not called on stop verdict")
	}
}

func newTestTasks(t *testing.T) (*task.Manager, task.Task) {
	t.Helper()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	tasks := task.NewManager(store, nil)
	tk, err := tasks.Create("watchdog test", "", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInProgress)})
	if err != nil {
		t.Fatalf("set in-progress: %v", err)
	}
	return tasks, tk
}
