package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
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

// fakeManualTestConfigGetter stands in for the real project/repo resolver so
// the test exercises the same hydration path ResumeStalled uses in
// production (taskToInfo never populates ManualTest — only
// withManualTestConfig does), instead of masking the hydration bug by
// hand-populating TaskInfo.ManualTest before calling handleWatchdogHangRetry.
type fakeManualTestConfigGetter map[string]ManualTestInfo

func (f fakeManualTestConfigGetter) ManualTestConfig(taskID string) ManualTestInfo { return f[taskID] }

func TestHandleWatchdogHangRetry_RunTestPrioritizesManualTestSurface(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	manualTest := ManualTestInfo{
		Kind:          "server",
		Command:       "go run ./cmd/sybra-server",
		HealthURL:     "http://127.0.0.1:8080/health",
		ProbeCommands: []string{"curl -fsS http://127.0.0.1:8080/health"},
	}
	engine.SetManualTestConfigGetter(fakeManualTestConfigGetter{"t1": manualTest})
	wf := &Execution{
		WorkflowID:  "testing-task",
		CurrentStep: testVerdictSourceStep,
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "testing",
		StatusReason: "watchdog hang: no stream activity",
		Workflow:     wf,
	})
	// Unhydrated, matching what ListTasks/taskToInfo hands ResumeStalled in
	// production — handleWatchdogHangRetry must hydrate ManualTest itself.
	ti := TaskInfo{
		ID:           "t1",
		Status:       "testing",
		StatusReason: "watchdog hang: no stream activity",
		Workflow:     wf,
	}

	escalated := engine.handleWatchdogHangRetry(&ti, &Step{ID: testVerdictSourceStep, Type: StepRunAgent, Config: StepConfig{Role: testRunnerRole}})
	if escalated {
		t.Fatal("first run_test hang should retry, not escalate")
	}
	// The run_test prompt (testing-task.yaml) reads testing_reask_note, so the
	// hang guidance must land there — not in watchdog_reask_note, which only the
	// implementation prompt consumes.
	if stray := wf.Variables[watchdogReaskNoteVar]; stray != "" {
		t.Fatalf("run_test hang note must not land in watchdog_reask_note:\n%s", stray)
	}
	note := wf.Variables[testingReaskNoteVar]
	for _, want := range []string{
		"manual_test surface",
		"go run ./cmd/sybra-server",
		"http://127.0.0.1:8080/health",
		"curl -fsS http://127.0.0.1:8080/health",
		"Before any further repo reading",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("reask note missing %q:\n%s", want, note)
		}
	}
}

func TestHandleWatchdogHangRetry_NonReadyPRStillRetries(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetPRStateFetcher(scriptedPRStateFetcher{state: github.PRState{State: "OPEN", Mergeable: "CONFLICTING"}})
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog hang: no stream activity",
		ProjectID:    "owner/repo",
		PRNumber:     42,
		Workflow:     wf,
	})
	ti := TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog hang: no stream activity",
		ProjectID:    "owner/repo",
		PRNumber:     42,
		Workflow:     wf,
	}

	escalated := engine.handleWatchdogHangRetry(&ti, &Step{ID: "implement", Type: StepRunAgent})
	if escalated {
		t.Fatal("non-ready PR should fall back to the normal retry path")
	}
	if got := wf.Variables[watchdogHangRetryKey("implement")]; got != "1" {
		t.Fatalf("hang retry var = %q, want %q", got, "1")
	}
}

func TestResumeStalled_WatchdogHangReadyPRSkipsRedispatch(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(newStoreWithBuiltin(t, "simple-task-implement"), tasks, agents, discardLogger())
	engine.SetPRStateFetcher(scriptedPRStateFetcher{state: github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}})
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables: map[string]string{
			watchdogReaskNoteVar: "stale watchdog retry note",
		},
		StartedAt: time.Now().UTC(),
	}
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog hang: no stream activity",
		ProjectID:    "owner/repo",
		PRNumber:     42,
		Workflow:     wf,
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "in-review" {
		t.Fatalf("status = %q, want %q", got.Status, "in-review")
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want empty", got.StatusReason)
	}
	if got.Workflow == nil {
		t.Fatal("workflow = nil, want completed workflow persisted")
	}
	if got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %q, want %q", got.Workflow.State, ExecCompleted)
	}
	if got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow current_step = %q, want empty", got.Workflow.CurrentStep)
	}
	if got.Workflow.CompletedAt == nil {
		t.Fatal("workflow completed_at not set")
	}
	if got.Workflow.Variables[watchdogHangRetryKey("implement")] != "" {
		t.Fatalf("hang retry var = %q, want empty (no redispatch budget consumed)", got.Workflow.Variables[watchdogHangRetryKey("implement")])
	}
	if got.Workflow.Variables[watchdogReaskNoteVar] != "" {
		t.Fatalf("watchdog note = %q, want cleared", got.Workflow.Variables[watchdogReaskNoteVar])
	}
	if got.Workflow.Variables["cancel_reason"] != "watchdog hang: implementation superseded by linked PR already open and green" {
		t.Fatalf("cancel_reason = %q", got.Workflow.Variables["cancel_reason"])
	}
}

func TestBuildWatchdogReaskNote_AttemptCount(t *testing.T) {
	t.Parallel()
	if got := buildWatchdogReaskNote(2); !strings.Contains(got, "attempt 2 of 2") {
		t.Fatalf("buildWatchdogReaskNote(2) = %q", got)
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
