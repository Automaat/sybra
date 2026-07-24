package workflow

import (
	"testing"
	"time"
)

// TestRewindRetry_PolicyMatrix exercises rewindRetry's shared shape in
// isolation from any concrete policy: arm-vs-exhaust branching, the rewind
// mechanics (counter, backoff, onArm, step-history clear, CurrentStep/State),
// and the persist-error contract. Concrete policies (testing auto-retry,
// verify-checks auto-fix, focused-checks auto-fix) each get their own
// behavioral tests; this one guards the shared skeleton they're all built on.
func TestRewindRetry_PolicyMatrix(t *testing.T) {
	newExec := func(counter string) *Execution {
		wf := &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "run_test",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		}
		if counter != "" {
			wf.Variables["retry.count"] = counter
		}
		return wf
	}

	t.Run("under cap arms and rewinds", func(t *testing.T) {
		tasks := newMemTasks()
		engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		wf := newExec("")
		ti := TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf}
		tasks.Put(ti)

		var onArmAttempt int
		var reasonAttempt int
		armed, attempt, err := engine.rewindRetry("t1", wf, ti, rewindRetryPolicy{
			counterKey: "retry.count",
			max:        2,
			rewindStep: "implement",
			backoff:    func(int) time.Duration { return time.Minute },
			onArm: func(wfExec *Execution, attempt int) {
				onArmAttempt = attempt
				wfExec.SetVar("side_effect", "set")
			},
			reason: func(attempt int) string {
				reasonAttempt = attempt
				return "retrying"
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !armed {
			t.Fatal("under cap must arm")
		}
		if attempt != 1 || onArmAttempt != 1 || reasonAttempt != 1 {
			t.Fatalf("attempt/onArmAttempt/reasonAttempt = %d/%d/%d, want 1/1/1", attempt, onArmAttempt, reasonAttempt)
		}
		if wf.Variables["retry.count"] != "1" {
			t.Fatalf("retry counter = %q, want 1", wf.Variables["retry.count"])
		}
		if wf.Variables["side_effect"] != "set" {
			t.Fatal("onArm side effect was not applied")
		}
		if wf.Variables[workflowRetryAfterVar] == "" {
			t.Fatal("retry-after not set")
		}
		if wf.CurrentStep != "implement" {
			t.Fatalf("CurrentStep = %q, want implement", wf.CurrentStep)
		}
		if wf.State != ExecWaiting {
			t.Fatalf("State = %q, want ExecWaiting", wf.State)
		}
		fresh, err := tasks.GetTask("t1")
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if fresh.Workflow.Variables["retry.count"] != "1" {
			t.Fatal("rewind was not persisted")
		}
		if fresh.StatusReason != "retrying" {
			t.Fatalf("status reason = %q, want retrying", fresh.StatusReason)
		}
	})

	t.Run("at cap does not arm and leaves counter untouched", func(t *testing.T) {
		tasks := newMemTasks()
		engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		wf := newExec("2")
		ti := TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf}
		tasks.Put(ti)

		onArmCalled := false
		armed, attempt, err := engine.rewindRetry("t1", wf, ti, rewindRetryPolicy{
			counterKey: "retry.count",
			max:        2,
			rewindStep: "implement",
			backoff:    func(int) time.Duration { return time.Minute },
			onArm:      func(*Execution, int) { onArmCalled = true },
			reason:     func(int) string { return "retrying" },
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if armed {
			t.Fatal("at cap must not arm")
		}
		if attempt != 2 {
			t.Fatalf("attempt = %d, want 2 (unchanged)", attempt)
		}
		if onArmCalled {
			t.Fatal("onArm must not fire once the cap is spent")
		}
		if wf.Variables["retry.count"] != "2" {
			t.Fatalf("retry counter = %q, want unchanged 2", wf.Variables["retry.count"])
		}
		if wf.CurrentStep != "run_test" {
			t.Fatalf("CurrentStep = %q, want unchanged run_test", wf.CurrentStep)
		}
	})

	t.Run("persist failure reports armed with error", func(t *testing.T) {
		tasks := newMemTasks()
		tasks.failSetWorkflow = true
		engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
		wf := newExec("")
		ti := TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf}
		tasks.Put(ti)

		armed, attempt, err := engine.rewindRetry("t1", wf, ti, rewindRetryPolicy{
			counterKey: "retry.count",
			max:        2,
			rewindStep: "implement",
			backoff:    func(int) time.Duration { return time.Minute },
			reason:     func(int) string { return "retrying" },
		})
		if err == nil {
			t.Fatal("expected persist error")
		}
		if !armed {
			t.Fatal("armed must be true even when persist fails — the counter bump already happened")
		}
		if attempt != 1 {
			t.Fatalf("attempt = %d, want 1", attempt)
		}
	})
}
