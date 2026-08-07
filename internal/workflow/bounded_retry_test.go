package workflow

import (
	"errors"
	"testing"
)

// TestBoundedRetry_PolicyMatrix exercises boundedRetry's shared shape in
// isolation from any concrete policy: applies/busy gating, arm-vs-exhaust
// branching, and the persist/onArmed error short-circuits. Concrete policies
// (watchdog hang/reward-hacking/rate-limit/stop, worktree-repair,
// transient-fetch) each get their own behavioral tests; this one guards the
// shared skeleton they're all built on.
func TestBoundedRetry_PolicyMatrix(t *testing.T) {
	newTaskStep := func(counter string) (*TaskInfo, *Step) {
		ti := &TaskInfo{
			ID:     "t1",
			Status: "in-progress",
			Workflow: &Execution{
				WorkflowID:  "test-simple",
				CurrentStep: "implement",
				State:       ExecWaiting,
				Variables:   map[string]string{},
			},
		}
		if counter != "" {
			ti.Workflow.Variables["retry.count"] = counter
		}
		return ti, &Step{ID: "implement", Type: StepRunAgent}
	}

	t.Run("applies false is a no-op", func(t *testing.T) {
		tasks := newMemTasks()
		engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		ti, step := newTaskStep("")
		tasks.Put(*ti)

		armed, exhausted := false, false
		handled := engine.boundedRetry(ti, step, boundedRetryPolicy{
			name:        "test-policy",
			applies:     func(*Engine, *TaskInfo, *Step) bool { return false },
			counterKey:  func(string) string { return "retry.count" },
			max:         1,
			onArm:       func(*Engine, *TaskInfo, *Step, int) { armed = true },
			onExhausted: func(*Engine, *TaskInfo, *Step, int) { exhausted = true },
		})
		if handled || armed || exhausted {
			t.Fatalf("applies=false must be a pure no-op: handled=%v armed=%v exhausted=%v", handled, armed, exhausted)
		}
	})

	t.Run("busy skips without consuming budget", func(t *testing.T) {
		tasks := newMemTasks()
		engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		ti, step := newTaskStep("")
		tasks.Put(*ti)

		handled := engine.boundedRetry(ti, step, boundedRetryPolicy{
			name:       "test-policy",
			applies:    func(*Engine, *TaskInfo, *Step) bool { return true },
			busy:       func(*Engine, *TaskInfo, *Step) bool { return true },
			counterKey: func(string) string { return "retry.count" },
			max:        1,
		})
		if handled {
			t.Fatal("busy=true must not be treated as handled")
		}
		if ti.Workflow.Variables["retry.count"] != "" {
			t.Fatalf("busy=true must not touch the counter, got %q", ti.Workflow.Variables["retry.count"])
		}
	})

	t.Run("under cap arms a retry", func(t *testing.T) {
		tasks := newMemTasks()
		engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		ti, step := newTaskStep("")
		tasks.Put(*ti)

		var armedAttempt, armedAttemptOnArmed int
		handled := engine.boundedRetry(ti, step, boundedRetryPolicy{
			name:       "test-policy",
			applies:    func(*Engine, *TaskInfo, *Step) bool { return true },
			counterKey: func(string) string { return "retry.count" },
			max:        2,
			onArm: func(_ *Engine, t *TaskInfo, _ *Step, attempt int) {
				armedAttempt = attempt
				t.Workflow.SetVar("side_effect", "set")
			},
			onArmed: func(e *Engine, t *TaskInfo, _ *Step, attempt int) error {
				armedAttemptOnArmed = attempt
				return e.tasks.UpdateTaskStatus(t.ID, "in-progress", "")
			},
		})
		if handled {
			t.Fatal("a fresh retry under cap must not be reported as handled/exhausted")
		}
		if armedAttempt != 1 || armedAttemptOnArmed != 1 {
			t.Fatalf("onArm/onArmed attempt = %d/%d, want 1/1", armedAttempt, armedAttemptOnArmed)
		}
		fresh, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if fresh.Workflow.Variables["retry.count"] != "1" {
			t.Fatalf("retry counter = %q, want %q", fresh.Workflow.Variables["retry.count"], "1")
		}
		if fresh.Workflow.Variables["side_effect"] != "set" {
			t.Fatal("onArm side effect was not persisted")
		}
	})

	t.Run("at cap escalates via onExhausted and never arms", func(t *testing.T) {
		tasks := newMemTasks()
		engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		ti, step := newTaskStep("2")
		tasks.Put(*ti)

		var gotAttempts int
		armCalled := false
		handled := engine.boundedRetry(ti, step, boundedRetryPolicy{
			name:       "test-policy",
			applies:    func(*Engine, *TaskInfo, *Step) bool { return true },
			counterKey: func(string) string { return "retry.count" },
			max:        2,
			onArm:      func(*Engine, *TaskInfo, *Step, int) { armCalled = true },
			onExhausted: func(e *Engine, t *TaskInfo, _ *Step, attempts int) {
				gotAttempts = attempts
				_ = e.tasks.UpdateTaskStatus(t.ID, "human-required", "exhausted")
			},
		})
		if !handled {
			t.Fatal("budget spent must be reported as handled")
		}
		if armCalled {
			t.Fatal("onArm must not fire once the budget is exhausted")
		}
		if gotAttempts != 2 {
			t.Fatalf("onExhausted attempts = %d, want 2", gotAttempts)
		}
		fresh, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if fresh.Status != "human-required" {
			t.Fatalf("status = %q, want human-required", fresh.Status)
		}
	})

	t.Run("persist error uses onPersistError override instead of the default log", func(t *testing.T) {
		tasks := newMemTasks()
		tasks.failSetWorkflow = true
		engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		ti, step := newTaskStep("")
		tasks.Put(*ti)

		var gotErr error
		handled := engine.boundedRetry(ti, step, boundedRetryPolicy{
			name:       "test-policy",
			applies:    func(*Engine, *TaskInfo, *Step) bool { return true },
			counterKey: func(string) string { return "retry.count" },
			max:        2,
			onPersistError: func(_ *Engine, _ *TaskInfo, _ *Step, err error) {
				gotErr = err
			},
		})
		if !handled {
			t.Fatal("persist failure must be reported as handled")
		}
		if gotErr == nil {
			t.Fatal("onPersistError override was not invoked")
		}
	})

	t.Run("onArmed error is reported as handled without a retry log", func(t *testing.T) {
		tasks := newMemTasks()
		engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		ti, step := newTaskStep("")
		tasks.Put(*ti)

		handled := engine.boundedRetry(ti, step, boundedRetryPolicy{
			name:       "test-policy",
			applies:    func(*Engine, *TaskInfo, *Step) bool { return true },
			counterKey: func(string) string { return "retry.count" },
			max:        2,
			onArmed: func(*Engine, *TaskInfo, *Step, int) error {
				return errBoundedRetryTest
			},
		})
		if !handled {
			t.Fatal("onArmed failure must still short-circuit as handled")
		}
	})
}

var errBoundedRetryTest = errors.New("boom")
