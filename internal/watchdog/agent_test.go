package watchdog

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
)

func TestStallLimit(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want time.Duration
	}{
		{"small", []string{"small"}, 10 * time.Minute},
		{"medium", []string{"medium"}, 15 * time.Minute},
		{"unset", nil, 15 * time.Minute},
		{"large", []string{"large"}, 45 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stallLimit(tc.tags)
			if got != tc.want {
				t.Fatalf("stallLimit(%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

func TestApplyVerdict_EscalateLeavesTaskRunning(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "stall", agent.InspectorVerdict{
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
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "loop", agent.InspectorVerdict{
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

func TestApplyVerdict_StallStopMarksRetryableHang(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:     tasks,
		logger:    slog.New(slog.DiscardHandler),
		stopAgent: func(string) error { stopped = true; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "stall", agent.InspectorVerdict{
		Stuck:          true,
		Reason:         "no stream activity",
		Recommendation: "stop",
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
	if got.StatusReason != "watchdog hang: no stream activity" {
		t.Fatalf("status_reason = %q, want retryable watchdog hang marker", got.StatusReason)
	}
	if !stopped {
		t.Fatal("stopAgent not called on stall stop verdict")
	}
}

func TestDecideTrigger(t *testing.T) {
	const (
		sl     = 15 * time.Minute
		budget = 45 * time.Minute
		thresh = 6
	)
	tests := []struct {
		name              string
		streak, threshold int
		acked             bool
		stall, total      time.Duration
		want              string
	}{
		{"none", 1, thresh, false, time.Minute, time.Minute, ""},
		{"loop over threshold", thresh, thresh, false, time.Minute, time.Minute, "loop"},
		{"loop below threshold", thresh - 1, thresh, false, time.Minute, time.Minute, ""},
		{"loop disabled by zero threshold", 100, 0, false, time.Minute, time.Minute, ""},
		{"loop wins over stall", thresh, thresh, false, 30 * time.Minute, time.Minute, "loop"},
		{"acked loop suppressed, none left", thresh, thresh, true, time.Minute, time.Minute, ""},
		{"acked loop falls through to stall", thresh, thresh, true, 30 * time.Minute, time.Minute, "stall"},
		{"stall", 0, thresh, false, 20 * time.Minute, time.Minute, "stall"},
		{"budget", 0, thresh, false, time.Minute, time.Hour, "budget"},
		{"stall wins over budget", 0, thresh, false, 20 * time.Minute, time.Hour, "stall"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideTrigger(tc.streak, tc.threshold, tc.acked, tc.stall, sl, tc.total, budget)
			if got != tc.want {
				t.Fatalf("decideTrigger = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyVerdict_NudgeLiveTransportDeliversInPlace covers the live-transport
// path: an agent with a working SendPromptToAgent (interactive/conversational)
// is steered in place and left running — no stop, no persisted steer.
func TestApplyVerdict_NudgeLiveTransportDeliversInPlace(t *testing.T) {
	tasks, tk := newTestTasks(t)

	var nudged string
	stopped := false
	w := &Watchdog{
		tasks:      tasks,
		logger:     slog.New(slog.DiscardHandler),
		stopAgent:  func(string) error { stopped = true; return nil },
		nudgeAgent: func(_, text string) error { nudged = text; return nil },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "loop", agent.InspectorVerdict{
		Recommendation: "nudge",
		Reason:         "drifting",
		Nudge:          "fix the root cause first",
	})

	if nudged != "⚠️ Supervisor: fix the root cause first" {
		t.Fatalf("nudge message = %q", nudged)
	}
	if stopped {
		t.Fatal("stopAgent called on a live-transport nudge")
	}
	got, _ := tasks.Get(tk.ID)
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (nudge must not flip status)", got.Status)
	}
	if got.SupervisorSteer != "" {
		t.Fatalf("supervisor_steer = %q, want empty for a live nudge", got.SupervisorSteer)
	}
}

// TestApplyVerdict_NudgeHeadlessPersistsSteerAndStops covers the headless path:
// no live transport, so the steer is persisted on the task and the agent is
// stopped so the recovery loop re-dispatches with the correction. The task is
// left in-progress (not human-required) so recovery resumes it.
func TestApplyVerdict_NudgeHeadlessPersistsSteerAndStops(t *testing.T) {
	tasks, tk := newTestTasks(t)

	stopped := false
	w := &Watchdog{
		tasks:      tasks,
		logger:     slog.New(slog.DiscardHandler),
		stopAgent:  func(string) error { stopped = true; return nil },
		nudgeAgent: func(_, _ string) error { return errors.New("no active transport") },
	}

	w.applyVerdict(&agent.Agent{ID: "a1", TaskID: tk.ID}, "loop", agent.InspectorVerdict{
		Recommendation: "nudge",
		Reason:         "drifting",
		Nudge:          "stop retrying the failing command",
	})

	if !stopped {
		t.Fatal("headless nudge must stop the agent so recovery re-dispatches")
	}
	got, _ := tasks.Get(tk.ID)
	if got.SupervisorSteer != "stop retrying the failing command" {
		t.Fatalf("supervisor_steer = %q, want the steer persisted", got.SupervisorSteer)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (recovery must resume, not park)", got.Status)
	}
}

func TestInspect_LoopAckOnlyWhenLeftRunning(t *testing.T) {
	tests := []struct {
		name    string
		verdict string
		wantAck bool
	}{
		{"continue acks the loop", "continue", true},
		{"escalate acks the loop", "escalate", true},
		{"stop does not ack (stop may fail, keep inspecting)", "stop", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tasks, tk := newTestTasks(t)
			w := &Watchdog{
				tasks:     tasks,
				logger:    slog.New(slog.DiscardHandler),
				emit:      func(string, any) {},
				stopAgent: func(string) error { return nil },
				inspectAgent: func(context.Context, *slog.Logger, agent.InspectInput) (agent.InspectorVerdict, error) {
					return agent.InspectorVerdict{Recommendation: tc.verdict, Reason: "x"}, nil
				},
			}
			ag := &agent.Agent{ID: "a1", TaskID: tk.ID}
			ag.NoteToolSignature("sig") // arm a loop signature

			w.inspect(context.Background(), ag, tk, "loop", 1, 1)

			if got := ag.ToolLoopAcknowledged(); got != tc.wantAck {
				t.Fatalf("ToolLoopAcknowledged after %q = %v, want %v", tc.verdict, got, tc.wantAck)
			}
		})
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
