package workflow

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/dispatchorder"
	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestMatchWorkflow_ReviewTag(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Task WITH review tag should NOT match test-simple.
	review := TaskInfo{ID: "t1", Tags: []string{"review"}}
	if def := engine.MatchWorkflow(review, "task.created"); def != nil {
		t.Fatalf("expected no match for review tag, got %s", def.ID)
	}

	// Task WITHOUT review tag should match.
	normal := TaskInfo{ID: "t2", Tags: []string{"backend"}}
	if def := engine.MatchWorkflow(normal, "task.created"); def == nil {
		t.Fatal("expected match for normal task")
	}
}

func TestMatchWorkflow_NoMatch(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Wrong event type.
	normal := TaskInfo{ID: "t1"}
	if def := engine.MatchWorkflow(normal, "pr.event"); def != nil {
		t.Fatalf("expected no match for pr.event, got %s", def.ID)
	}
}

func TestDispatchEvent_MatchesAndStarts(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{ID: "t1", Status: "in-review", AgentMode: "headless"})

	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix the thing"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "pr-fix-test" {
		t.Fatalf("wfID = %q, want pr-fix-test", wfID)
	}
	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent call, got %d", agents.CallCount())
	}
	if got := agents.LastCall().Prompt; got != "fix the thing" {
		t.Errorf("prompt = %q, want 'fix the thing'", got)
	}
}

func TestDispatchEvent_NoMatchReturnsEmpty(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	// Extra fields miss the condition (kind=conflict, workflow wants ci_failure).
	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "conflict"}, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "" {
		t.Fatalf("wfID = %q, want empty", wfID)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("expected no agent calls, got %d", agents.CallCount())
	}
}

func TestDispatchEvent_AlreadyActiveRejected(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "implement",
			State:       ExecWaiting,
		},
	})

	_, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"}, nil)
	if !errors.Is(err, ErrWorkflowAlreadyActive) {
		t.Fatalf("expected ErrWorkflowAlreadyActive, got %v", err)
	}
	if agents.CallCount() != 0 {
		t.Fatalf("expected no agent start on rejected dispatch, got %d", agents.CallCount())
	}
}

func TestDispatchEvent_TerminalWorkflowReplaced(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-review",
		Workflow: &Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "",
			State:       ExecCompleted, // terminal — dispatch should replace
		},
	})

	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "pr-fix-test" {
		t.Fatalf("wfID = %q, want pr-fix-test", wfID)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.WorkflowID != "pr-fix-test" {
		t.Errorf("workflow on task = %q, want pr-fix-test", ti.Workflow.WorkflowID)
	}
}

func TestHasActiveWorkflow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "no-wf", Status: "todo"})
	tasks.Put(TaskInfo{
		ID: "running", Status: "in-progress",
		Workflow: &Execution{WorkflowID: "x", State: ExecRunning},
	})
	tasks.Put(TaskInfo{
		ID: "waiting", Status: "in-progress",
		Workflow: &Execution{WorkflowID: "x", State: ExecWaiting},
	})
	tasks.Put(TaskInfo{
		ID: "completed", Status: "done",
		Workflow: &Execution{WorkflowID: "x", State: ExecCompleted},
	})
	tasks.Put(TaskInfo{
		ID: "failed", Status: "human-required",
		Workflow: &Execution{WorkflowID: "x", State: ExecFailed},
	})

	cases := []struct {
		id   string
		want bool
	}{
		{"no-wf", false},
		{"running", true},
		{"waiting", true},
		{"completed", false},
		{"failed", false},
		{"unknown-task", false},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			t.Parallel()
			if got := engine.HasActiveWorkflow(c.id); got != c.want {
				t.Errorf("HasActiveWorkflow(%q) = %v, want %v", c.id, got, c.want)
			}
		})
	}
}

func TestCancelWorkflow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID: "active", Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "pr-fix",
			CurrentStep: "fix",
			State:       ExecWaiting,
			Variables:   map[string]string{"pr_issue_kind": "ci_failure"},
		},
	})
	tasks.Put(TaskInfo{
		ID: "completed", Status: "done",
		Workflow: &Execution{WorkflowID: "pr-fix", State: ExecCompleted},
	})
	tasks.Put(TaskInfo{ID: "no-wf", Status: "todo"})

	// Pretend an agent is running for "active" so we can verify it's stopped.
	if _, _, _, err := agents.StartAgent("active", "pr-fix", "headless", "sonnet", "claude", "p", "", nil, false, false, "", "", AgentAssignment{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	step, err := engine.CancelWorkflow("active", "ci_failure resolved")
	if err != nil {
		t.Fatalf("CancelWorkflow active: %v", err)
	}
	if step != "fix" {
		t.Errorf("returned step = %q, want %q", step, "fix")
	}
	got, err := tasks.GetTask("active")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if got.Workflow.State != ExecCompleted {
		t.Errorf("State = %s, want %s", got.Workflow.State, ExecCompleted)
	}
	if got.Workflow.CurrentStep != "" {
		t.Errorf("CurrentStep = %q, want empty", got.Workflow.CurrentStep)
	}
	if got.Workflow.CompletedAt == nil {
		t.Error("CompletedAt not set")
	}
	if got.Workflow.Variables["cancel_reason"] != "ci_failure resolved" {
		t.Errorf("cancel_reason = %q", got.Workflow.Variables["cancel_reason"])
	}
	if agents.HasRunningAgent("active") {
		t.Error("agent still running after cancel")
	}

	// Already-terminal workflow → no-op, no error.
	if step, err := engine.CancelWorkflow("completed", "x"); err != nil || step != "" {
		t.Errorf("cancel completed: step=%q err=%v", step, err)
	}

	// No workflow attached → no-op, no error.
	if step, err := engine.CancelWorkflow("no-wf", "x"); err != nil || step != "" {
		t.Errorf("cancel no-wf: step=%q err=%v", step, err)
	}
}

func TestStartWorkflowRejectsTamperFlaggedRestart(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	originalWorkflow := &Execution{
		WorkflowID: "simple-task-implement",
		State:      ExecCompleted,
		Variables:  map[string]string{tamperDeletionAllowlistVar: `{"exact_paths":{},"basenames":{}}`},
	}
	tasks.Put(TaskInfo{
		ID:           "tamper",
		Status:       "human-required",
		StatusReason: TamperFlaggedReasonPrefix + " removed tests/foo_test.go",
		Blocker:      blocker.State{Kind: blocker.KindTamperDetected, Actor: blocker.ActorWorkflow},
		Tags:         []string{TamperBlessedTag},
		Workflow:     originalWorkflow,
	})

	err := engine.StartWorkflow("tamper", "test-simple")
	if !errors.Is(err, ErrTamperBlessRequired) {
		t.Fatalf("StartWorkflow err = %v, want ErrTamperBlessRequired", err)
	}
	got, getErr := tasks.GetTask("tamper")
	if getErr != nil {
		t.Fatalf("get task: %v", getErr)
	}
	if got.Workflow.WorkflowID != originalWorkflow.WorkflowID ||
		got.Workflow.State != originalWorkflow.State ||
		got.Workflow.CurrentStep != originalWorkflow.CurrentStep {
		t.Fatalf("workflow changed: %+v, want original %+v", got.Workflow, originalWorkflow)
	}
	if agents.HasRunningAgent("tamper") {
		t.Fatal("StartWorkflow launched an agent for a tamper-flagged task")
	}
}

func TestStartWorkflowAllowsNonTamperHumanRequiredRestart(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:           "human",
		Status:       "human-required",
		StatusReason: "operator needs more context",
		Workflow:     &Execution{WorkflowID: "old", State: ExecCompleted},
	})

	if err := engine.StartWorkflow("human", "test-simple"); err != nil {
		t.Fatalf("StartWorkflow err = %v, want nil", err)
	}
	if !agents.HasRunningAgent("human") {
		t.Fatal("StartWorkflow did not launch an agent for non-tamper human-required task")
	}
}

func TestStartWorkflow_PinsDefinitionHashAndSnapshot(t *testing.T) {
	t.Parallel()

	store := newInlineTestStore(t, "pin-test", `
id: pin-test
name: Pin Test
trigger:
  on: task.created
steps:
  - id: wait
    name: Wait
    type: wait_human
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine, err := NewEngine(store, tasks, agents, discardLogger(), completeDependencies())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	if err := engine.StartWorkflow("t1", "pin-test"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil {
		t.Fatal("workflow not persisted")
	}
	if ti.Workflow.DefinitionHash == "" {
		t.Fatal("DefinitionHash not stamped")
	}
	def, err := store.Get("pin-test")
	if err != nil {
		t.Fatalf("Get workflow: %v", err)
	}
	wantHash, err := def.SemanticHash()
	if err != nil {
		t.Fatalf("SemanticHash: %v", err)
	}
	if ti.Workflow.DefinitionHash != wantHash {
		t.Fatalf("DefinitionHash = %q, want %q", ti.Workflow.DefinitionHash, wantHash)
	}
	if _, err := store.GetSnapshot("pin-test", ti.Workflow.DefinitionHash); err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
}

func TestStartWorkflow_NewTaskUsesLatestDefinitionAfterRewrite(t *testing.T) {
	t.Parallel()

	store := newInlineTestStore(t, "pin-test", `
id: pin-test
name: Pin Test
trigger:
  on: task.created
steps:
  - id: wait
    name: Wait Original
    type: wait_human
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine, err := NewEngine(store, tasks, agents, discardLogger(), completeDependencies())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	if err := engine.StartWorkflow("t1", "pin-test"); err != nil {
		t.Fatalf("StartWorkflow first: %v", err)
	}
	first, _ := tasks.GetTask("t1")

	if err := store.Save(Definition{
		ID:   "pin-test",
		Name: "Pin Test",
		Trigger: Trigger{
			On: "task.created",
		},
		Steps: []Step{{
			ID:   "wait",
			Name: "Wait Updated",
			Type: StepWaitHuman,
		}},
	}); err != nil {
		t.Fatalf("Save updated workflow: %v", err)
	}

	tasks.Put(TaskInfo{ID: "t2", Status: "todo"})
	if err := engine.StartWorkflow("t2", "pin-test"); err != nil {
		t.Fatalf("StartWorkflow second: %v", err)
	}
	second, _ := tasks.GetTask("t2")
	if first.Workflow.DefinitionHash == second.Workflow.DefinitionHash {
		t.Fatalf("new task reused old definition hash %q after rewrite", second.Workflow.DefinitionHash)
	}
	def, err := store.Get("pin-test")
	if err != nil {
		t.Fatalf("Get latest workflow: %v", err)
	}
	latestHash, err := def.SemanticHash()
	if err != nil {
		t.Fatalf("SemanticHash latest: %v", err)
	}
	if second.Workflow.DefinitionHash != latestHash {
		t.Fatalf("second DefinitionHash = %q, want latest %q", second.Workflow.DefinitionHash, latestHash)
	}
}

func TestMatchWorkflow_PriorityTieBreak(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Two workflows match the same event + field — higher priority wins.
	addPREventWorkflow(t, store, "pr-fix-generic", 0, "ci_failure")
	addPREventWorkflow(t, store, "pr-fix-specialized", 10, "ci_failure")

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "pr-fix-specialized" {
		t.Errorf("wfID = %q, want pr-fix-specialized (priority 10 should beat 0)", wfID)
	}
}

func TestMatchWorkflow_EqualPriorityDeterministic(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Two workflows with equal priority — alphabetical (store order) wins.
	addPREventWorkflow(t, store, "pr-fix-zebra", 5, "ci_failure")
	addPREventWorkflow(t, store, "pr-fix-alpha", 5, "ci_failure")

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	wfID, err := engine.DispatchEvent("t1", "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "pr-fix-alpha" {
		t.Errorf("wfID = %q, want pr-fix-alpha (alphabetical tiebreak)", wfID)
	}
}

func TestDispatchPriorityRank(t *testing.T) {
	cases := []struct {
		status taskstatus.Status
		want   int
	}{
		{"in-review", 0},
		{"ready-pr", 0},
		{"ready-review", 1},
		{"testing", 1},
		{"in-progress", 2},
		{"planning", 3},
		{"plan-review", 3},
		{"todo", 4},
		{"new", 4},
		{"blocked", 4},
		{"", 4},
	}
	for _, tc := range cases {
		if got := dispatchorder.Rank(string(tc.status)); got != tc.want {
			t.Errorf("dispatchorder.Rank(%q) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

func TestStartWorkflow_InvalidWorkflowID(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	err := engine.StartWorkflow("t1", "nonexistent-workflow")
	if err == nil {
		t.Fatal("expected error for invalid workflow ID")
	}
}

// TestDispatchEvent_SkipsClaimHeldByOutOfBandDispatcher verifies DispatchEvent
// also treats a shared agent.Manager dispatch claim held for the task as busy,
// even when the workflow engine has no active local route for the task.
func TestDispatchEvent_SkipsClaimHeldByOutOfBandDispatcher(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	agents.SetDispatchClaimed("t1", true)

	_, err := engine.DispatchEvent("t1", "task.created", nil, nil)
	if !errors.Is(err, ErrWorkflowAlreadyActive) {
		t.Fatalf("DispatchEvent with shared claim held elsewhere: err = %v, want ErrWorkflowAlreadyActive", err)
	}
}

// TestStartWorkflow_InitialDispatchFailure_EscalatesPermanentError guards
// against the workflow.external-create.failed gap: a permanent dispatch
// error (e.g. ErrNoProjectAssigned) on the very FIRST execution of a
// workflow — StartWorkflow/DispatchEvent, not a ResumeStalled retry — must
// classify and escalate exactly like ResumeStalled already does. Before the
// fix, startWorkflowCore returned the raw error to its caller (who only logs
// it) and left the task's Workflow live/non-terminal, so the task silently
// sat in limbo until some later resume attempt happened to escalate it.
func TestStartWorkflow_InitialDispatchFailure_EscalatesPermanentError(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Todo, AgentMode: "headless"})
	agents := newMockAgents()
	agents.SetFailSpawn(fmt.Errorf("task t1 has no project_id: refusing to start triage agent without isolated worktree: %w", ErrNoProjectAssigned))
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	if err := engine.StartWorkflow("t1", "test-simple"); err == nil {
		t.Fatal("StartWorkflow should propagate the spawn error")
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required on the very first dispatch attempt", got.Status)
	}
	if !strings.Contains(got.StatusReason, "no project could be assigned") {
		t.Errorf("status reason = %q, want it to mention the classified no-project reason", got.StatusReason)
	}
}

// TestStartWorkflow_ConcurrentSameTaskSingleWinner verifies the per-task
// `starting` mutex serializes concurrent StartWorkflowWithVars calls for
// the same task. Exactly one caller wins; the others get
// ErrWorkflowAlreadyActive. Without the lock, both callers would spawn
// duplicate agents for the same task (the original bug this test pins).
func TestStartWorkflow_ConcurrentSameTaskSingleWinner(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Todo, AgentMode: "headless"})

	const callers = 5
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Go(func() {
			<-start
			errs[i] = engine.StartWorkflow("t1", "test-simple")
		})
	}
	close(start)
	wg.Wait()

	successCount := 0
	rejectedCount := 0
	for _, err := range errs {
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, ErrWorkflowAlreadyActive):
			rejectedCount++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successCount != 1 {
		t.Errorf("got %d successful starts, want exactly 1", successCount)
	}
	if rejectedCount != callers-1 {
		t.Errorf("got %d rejections, want %d (all losers must be rejected with ErrWorkflowAlreadyActive)", rejectedCount, callers-1)
	}

	// Exactly one agent was spawned — the bug this test guards against is
	// two concurrent callers both reaching executeSteps.
	if got := agents.CallCount(); got != 1 {
		t.Errorf("agent spawn count = %d, want 1 (lock should prevent duplicate spawns)", got)
	}

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Workflow == nil || ti.Workflow.WorkflowID != "test-simple" {
		t.Errorf("task workflow not set correctly: %+v", ti.Workflow)
	}
}

// atomic.Bool's zero value is false, so the documented default has to be
// stored explicitly — dropping that flips behaviour silently.
func TestNewEngine_OpenPROnUnrunnableGateDefaultsTrue(t *testing.T) {
	store := newTestStore(t)
	engine := NewTestEngine(store, newMemTasks(), newMockAgents(), discardLogger())
	if !engine.openPROnUnrunnableGate.Load() {
		t.Error("openPROnUnrunnableGate = false, want the documented default of true")
	}
}
