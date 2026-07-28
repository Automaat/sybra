package workflow

import (
	"strconv"
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

func TestResumeStalled_WatchdogHangRunTestRendersTestingReaskNote(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(*mustBuiltinDefinition(t, "testing-task")); err != nil {
		t.Fatalf("save testing-task: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "testing",
		StatusReason: "watchdog hang: no stream activity",
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "testing-task",
			CurrentStep: testVerdictSourceStep,
			State:       ExecWaiting,
			Variables:   map[string]string{},
			StartedAt:   time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", got)
	}
	prompt := agents.calls[0].Prompt
	if !strings.Contains(prompt, "RETRY GUIDANCE") {
		t.Fatalf("run_test prompt missing retry guidance marker:\n%s", prompt)
	}
	if !strings.Contains(prompt, "watchdog hang") {
		t.Fatalf("run_test prompt missing watchdog hang context:\n%s", prompt)
	}
	if !strings.Contains(prompt, "attempt 1 of 2") {
		t.Fatalf("run_test prompt missing watchdog attempt count:\n%s", prompt)
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

func TestHandleWatchdogHangRetry_RunTestExhaustionOpensPRGate(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "testing-task",
		CurrentStep: testVerdictSourceStep,
		State:       ExecWaiting,
		Variables:   map[string]string{watchdogHangRetryKey(testVerdictSourceStep): strconv.Itoa(maxWatchdogHangRetries)},
		StartedAt:   time.Now().UTC(),
	}
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "testing",
		StatusReason: "watchdog hang: no stream activity",
		Workflow:     wf,
	})
	ti := TaskInfo{ID: "t1", Status: "testing", StatusReason: "watchdog hang: no stream activity", Workflow: wf}

	handled := engine.handleWatchdogHangRetry(&ti, &Step{ID: testVerdictSourceStep, Type: StepRunAgent})
	if !handled {
		t.Fatal("exhausted run_test watchdog retry should be handled")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "ready-pr" {
		t.Fatalf("status = %q, want ready-pr", got.Status)
	}
	if got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %q, want ExecCompleted", got.Workflow.State)
	}
	if got.Workflow.CurrentStep != "" {
		t.Fatalf("current step = %q, want empty", got.Workflow.CurrentStep)
	}
	if got.Workflow.CompletedAt == nil {
		t.Fatal("completed_at should be set for exhausted run_test open-pr path")
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "harness/infra limitation") {
		t.Fatalf("reason = %q, want unrunnable gate reason", reason)
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

	escalated := engine.handleWatchdogRewardHackingRetry(&ti, &Step{ID: "fix_review", Type: StepRunAgent, Config: StepConfig{Role: "fix-review"}})
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
	if !strings.Contains(note, "code review sidecar already names the fix location") {
		t.Fatalf("reask note should point directly at the existing review finding:\n%s", note)
	}
	if !strings.Contains(note, "do not re-read unrelated files") {
		t.Fatalf("reask note should prohibit repeating unrelated file reads:\n%s", note)
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

func TestHandleWatchdogRewardHackingRetry_ImplementationUsesImplementationBudget(t *testing.T) {
	t.Parallel()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
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
		StatusReason: "watchdog: reward-hacking retry: re-reading the same files without changing code",
		Workflow:     wf,
	})
	ti := TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: reward-hacking retry: re-reading the same files without changing code",
		Workflow:     wf,
	}

	escalated := engine.handleWatchdogRewardHackingRetry(&ti, &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation"}})
	if escalated {
		t.Fatal("first implementation reward-hacking stop should retry, not escalate")
	}
	note := wf.Variables[watchdogReaskNoteVar]
	if !strings.Contains(note, "attempt 1 of 2") {
		t.Fatalf("reask note missing implementation attempt budget:\n%s", note)
	}
	if !strings.Contains(note, "NOTES.md") {
		t.Fatalf("reask note should steer implementation toward existing worktree context:\n%s", note)
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

	escalated := engine.handleWatchdogRewardHackingRetry(&ti, &Step{ID: "fix_review", Type: StepRunAgent, Config: StepConfig{Role: "fix-review"}})
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

func TestBuildRewardHackingFixReviewReaskNote_AttemptCount(t *testing.T) {
	t.Parallel()
	if got := buildRewardHackingFixReviewReaskNote(1); !strings.Contains(got, "attempt 1 of 1") {
		t.Fatalf("buildRewardHackingFixReviewReaskNote(1) = %q", got)
	}
}

func TestResumeStalled_WatchdogRewardHackingPlanCriticRendersRetryNote(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(*mustBuiltinDefinition(t, "simple-task-plan")); err != nil {
		t.Fatalf("save simple-task-plan: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "planning",
		StatusReason: "watchdog: reward-hacking retry: re-reading the plan without writing critique",
		Plan:         "# Execution Plan\n\n## Decision\nGrounded plan\n",
		PlanContract: `{"task_id":"t1","verification":[{"command":"go test ./...","expected":"pass"}],"acceptance_criteria":["done"]}`,
		AgentMode:    "headless",
		Workflow: &Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "critique_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
			StartedAt:   time.Now().UTC(),
		},
	})

	engine.ResumeStalled()

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", got)
	}
	prompt := agents.calls[0].Prompt
	if !strings.Contains(prompt, "watchdog detected a reward-hacking pattern") {
		t.Fatalf("critique_plan prompt missing reward-hacking context:\n%s", prompt)
	}
	if !strings.Contains(prompt, "plan-critique run") {
		t.Fatalf("critique_plan prompt missing stage-specific retry guidance:\n%s", prompt)
	}
}

// TestAdvanceStep_ClearsRewardHackingRetryOnFixReviewSuccess covers #2229's
// stop-and-reset promise: the retry counter must NOT survive a fix_review
// step that completes cleanly, since fix_review is re-entered fresh at the
// start of every subsequent review round (simple-task-review.yaml loops
// fix_review -> detect_tampering -> ... -> code_review -> fix_review). A
// reward_hacking stop on a later, unrelated round must retry once, not
// inherit an already-exhausted counter from an earlier round.
func TestAdvanceStep_ClearsRewardHackingRetryOnFixReviewSuccess(t *testing.T) {
	t.Parallel()
	const yaml = `
id: test-fixreview-reset
name: Test Fix Review Reset
trigger:
  on: task.status_changed
steps:
  - id: fix_review
    name: Fix Review
    type: run_agent
    config:
      role: fix-review
      mode: headless
    next:
      - goto: ""
`
	store := newInlineTestStore(t, "test-fixreview-reset", yaml)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wf := &Execution{
		WorkflowID:  "test-fixreview-reset",
		CurrentStep: "fix_review",
		State:       ExecWaiting,
		Variables: map[string]string{
			// Budget already spent by an earlier reward-hacking retry round.
			watchdogRewardHackingRetryKey("fix_review"): "1",
		},
		StartedAt: time.Now().UTC(),
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})

	if err := engine.AdvanceStep("t1", StepOutput{StepID: "fix_review", Status: "completed", Output: "fixed"}); err != nil {
		t.Fatalf("advance step: %v", err)
	}

	fresh, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if _, ok := fresh.Workflow.Variables[watchdogRewardHackingRetryKey("fix_review")]; ok {
		t.Fatal("reward-hacking retry counter should be cleared after a clean fix_review completion")
	}

	// A reward_hacking stop on a later round of the same step must retry
	// once, not escalate as if the budget were already exhausted.
	ti := TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "watchdog: reward-hacking retry: still looping",
		Workflow:     fresh.Workflow,
	}
	if escalated := engine.handleWatchdogRewardHackingRetry(&ti, &Step{ID: "fix_review", Type: StepRunAgent, Config: StepConfig{Role: "fix-review"}}); escalated {
		t.Fatal("reward-hacking retry budget should have reset after a successful round, not escalate immediately")
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
