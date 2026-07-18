package workflow

import (
	"strings"
	"testing"
	"time"
)

func TestHandleWatchdogHangRetry_SetsReaskNoteOnRetry(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog hang: no stream activity",
		Workflow:     wf,
	})
	ti := TaskInfo{ID: "t1", Status: "in-progress", StatusReason: "watchdog hang: no stream activity", Workflow: wf}

	escalated := engine.handleWatchdogHangRetry(&ti, &Step{ID: "implement", Type: StepRunAgent})
	if escalated {
		t.Fatal("first hang should retry, not escalate")
	}
	note := wf.Variables[watchdogReaskNoteVar]
	if !strings.Contains(note, "watchdog hang") {
		t.Fatalf("reask note missing hang context:\n%s", note)
	}
	if !strings.Contains(note, "attempt 1 of 2") {
		t.Fatalf("reask note missing attempt count:\n%s", note)
	}
	if !strings.Contains(note, "go test ./...") {
		t.Fatalf("reask note should steer the agent off the full suite:\n%s", note)
	}
	if !strings.Contains(note, "human-required") {
		t.Fatalf("reask note should offer the human-required escape hatch:\n%s", note)
	}
}

func TestBuildWatchdogReaskNote_AttemptCount(t *testing.T) {
	t.Parallel()
	if got := buildWatchdogReaskNote(2); !strings.Contains(got, "attempt 2 of 2") {
		t.Fatalf("buildWatchdogReaskNote(2) = %q", got)
	}
}

func TestHandleWatchdogRewardHackingRetry_SetsReaskNoteOnRetry(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "fix_review",
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: reward-hacking retry: repeating file reads without editing",
		Workflow:     wf,
	})
	ti := TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: reward-hacking retry: repeating file reads without editing",
		Workflow:     wf,
	}

	escalated := engine.handleWatchdogRewardHackingRetry(&ti, &Step{ID: "fix_review", Type: StepRunAgent})
	if escalated {
		t.Fatal("first reward-hacking stop should retry, not escalate")
	}
	note := wf.Variables[watchdogReaskNoteVar]
	if !strings.Contains(note, "reward-hacking") {
		t.Fatalf("reask note missing reward-hacking context:\n%s", note)
	}
	if !strings.Contains(note, "attempt 1 of 1") {
		t.Fatalf("reask note missing attempt count:\n%s", note)
	}
	if !strings.Contains(note, "sidecar already") {
		t.Fatalf("reask note should point at the sidecar's named location:\n%s", note)
	}
	if !strings.Contains(note, "human-required") {
		t.Fatalf("reask note should offer the human-required escape hatch:\n%s", note)
	}

	fresh, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if fresh.StatusReason != "" {
		t.Fatalf("status_reason = %q, want cleared so the workflow resumes cleanly", fresh.StatusReason)
	}
}

func TestHandleWatchdogRewardHackingRetry_ExhaustedBudgetEscalates(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "fix_review",
		State:       ExecWaiting,
		Variables:   map[string]string{watchdogRewardHackingRetryKey("fix_review"): "1"},
		StartedAt:   time.Now().UTC(),
	}
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: reward-hacking retry: still looping",
		Workflow:     wf,
	})
	ti := TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: reward-hacking retry: still looping",
		Workflow:     wf,
	}

	escalated := engine.handleWatchdogRewardHackingRetry(&ti, &Step{ID: "fix_review", Type: StepRunAgent})
	if !escalated {
		t.Fatal("exhausted reward-hacking retry budget should escalate")
	}
	fresh, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if fresh.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", fresh.Status)
	}
	if !strings.Contains(fresh.StatusReason, "retry budget exhausted") {
		t.Fatalf("status_reason = %q, want budget-exhausted explanation", fresh.StatusReason)
	}
	if wf.State != ExecFailed {
		t.Fatalf("workflow state = %q, want ExecFailed", wf.State)
	}
}

func TestBuildRewardHackingReaskNote_AttemptCount(t *testing.T) {
	t.Parallel()
	if got := buildRewardHackingReaskNote(1); !strings.Contains(got, "attempt 1 of 1") {
		t.Fatalf("buildRewardHackingReaskNote(1) = %q", got)
	}
}

func TestClearWatchdogReaskNote(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID: "test-simple",
		Variables:  map[string]string{watchdogReaskNoteVar: "stale hang guidance"},
		StartedAt:  time.Now().UTC(),
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})

	engine.clearWatchdogReaskNote("t1", wf)
	if _, ok := wf.Variables[watchdogReaskNoteVar]; ok {
		t.Fatal("watchdog reask note should be cleared on success")
	}
	engine.clearWatchdogReaskNote("t1", wf)
}
