package workflow

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Test helpers ---

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore creates a Store backed by a temp dir and copies the
// testdata/test-simple.yaml workflow into it.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return newTestStoreWith(t, "test-simple.yaml")
}

// newTestStoreWith copies one or more testdata yaml files into a fresh
// Store. Use this when a test needs a different workflow definition than
// the default test-simple.yaml.
func newTestStoreWith(t *testing.T, files ...string) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		src, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read test workflow %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// --- In-memory TaskProvider ---

type memTasks struct {
	mu    sync.Mutex
	tasks map[string]*TaskInfo
}

func newMemTasks() *memTasks {
	return &memTasks{tasks: make(map[string]*TaskInfo)}
}

func (m *memTasks) Put(t TaskInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = &t
}

func (m *memTasks) GetTask(id string) (TaskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return TaskInfo{}, fmt.Errorf("task %s not found", id)
	}
	return *t, nil
}

func (m *memTasks) ListTasks() ([]TaskInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TaskInfo
	for _, t := range m.tasks {
		out = append(out, *t)
	}
	return out, nil
}

func (m *memTasks) UpdateTaskStatus(id, status, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.Status = status
	return nil
}

func (m *memTasks) SetWorkflow(id string, wf *Execution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.Workflow = wf
	return nil
}

// SetStatus is a test helper to simulate an agent changing task status.
func (m *memTasks) SetStatus(id, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		t.Status = status
	}
}

// --- Mock AgentLauncher ---

type startCall struct {
	TaskID, Role, Mode, Model, Prompt, Dir string
	AllowedTools                           []string
	NeedsWorktree                          bool
}

type sentPrompt struct {
	AgentID, Message string
}

type mockAgents struct {
	mu      sync.Mutex
	calls   []startCall
	prompts []sentPrompt
	running map[string]string // taskID -> agentID
	roles   map[string]string // taskID+"/"+role -> agentID
	counter int
}

func newMockAgents() *mockAgents {
	return &mockAgents{
		running: make(map[string]string),
		roles:   make(map[string]string),
	}
}

func (m *mockAgents) StartAgent(taskID, role, mode, model, prompt, dir string, allowedTools []string, needsWorktree bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	id := fmt.Sprintf("agent-%d", m.counter)
	m.calls = append(m.calls, startCall{
		TaskID: taskID, Role: role, Mode: mode, Model: model,
		Prompt: prompt, Dir: dir, AllowedTools: allowedTools, NeedsWorktree: needsWorktree,
	})
	m.running[taskID] = id
	m.roles[taskID+"/"+role] = id
	return id, nil
}

func (m *mockAgents) HasRunningAgent(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.running[taskID]
	return ok
}

func (m *mockAgents) FindRunningAgentForRole(taskID, role string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.roles[taskID+"/"+role]
	return id, ok
}

func (m *mockAgents) StopAgentsForTask(taskID, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, taskID)
}

func (m *mockAgents) SendPrompt(agentID, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prompts = append(m.prompts, sentPrompt{AgentID: agentID, Message: message})
	return nil
}

// SimulateComplete marks the agent for a task as no longer running.
func (m *mockAgents) SimulateComplete(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, taskID)
}

// LastCall returns the most recent StartAgent call.
func (m *mockAgents) LastCall() startCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return startCall{}
	}
	return m.calls[len(m.calls)-1]
}

// LastID returns the most recent agent ID.
func (m *mockAgents) LastID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("agent-%d", m.counter)
}

// CallCount returns total StartAgent calls.
func (m *mockAgents) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// SentPrompts returns all recorded SendPrompt calls.
func (m *mockAgents) SentPrompts() []sentPrompt {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sentPrompt, len(m.prompts))
	copy(out, m.prompts)
	return out
}

// --- Tests ---

func TestFullLifecycle_DirectImplement(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage agent should have started.
	if agents.LastCall().Role != "triage" {
		t.Fatalf("expected triage, got %q", agents.LastCall().Role)
	}

	// Simulate triage completes — status stays "todo" → direct implement path.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "triaged"}); err != nil {
		t.Fatal(err)
	}

	// Should have advanced through set_in_progress → implement.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Fatalf("expected in-progress, got %q", ti.Status)
	}
	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation, got %q", agents.LastCall().Role)
	}

	// Simulate implement completes.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed", Output: "Done."}); err != nil {
		t.Fatal(err)
	}

	// Evaluate step.
	if agents.LastCall().Role != "eval" {
		t.Fatalf("expected eval, got %q", agents.LastCall().Role)
	}

	// Simulate evaluate completes.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "evaluate", Status: "completed", Output: "evaluated"}); err != nil {
		t.Fatal(err)
	}

	// Workflow should be completed.
	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("expected completed, got %q", ti.Workflow.State)
	}
}

func TestFullLifecycle_PlanPath(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage completes, agent set status to "planning".
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "needs planning"}); err != nil {
		t.Fatal(err)
	}

	// Plan agent started.
	if agents.LastCall().Role != "plan" {
		t.Fatalf("expected plan, got %q", agents.LastCall().Role)
	}
	if agents.LastCall().Mode != "interactive" {
		t.Fatalf("expected interactive, got %q", agents.LastCall().Mode)
	}

	// Plan agent completes → review_plan (wait_human).
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed", Output: "plan ready"}); err != nil {
		t.Fatal(err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "plan-review" {
		t.Fatalf("expected plan-review, got %q", ti.Status)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("expected waiting, got %q", ti.Workflow.State)
	}

	// Approve plan.
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	// Should advance through set_in_progress → implement.
	ti, _ = tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Fatalf("expected in-progress, got %q", ti.Status)
	}
	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation, got %q", agents.LastCall().Role)
	}

	// Implement → evaluate → done.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed", Output: "done"}); err != nil {
		t.Fatal(err)
	}
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "evaluate", Status: "completed", Output: "ok"}); err != nil {
		t.Fatal(err)
	}

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("expected completed, got %q", ti.Workflow.State)
	}
}

func TestPlanReject_ThenApprove(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage → planning path.
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})

	// Plan completes → wait_human.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed"})

	// Reject with feedback.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "needs more detail"}); err != nil {
		t.Fatal(err)
	}

	// Should go back to plan step. Since reuse_agent=true and the plan agent
	// is still in the roles map, it should send a prompt instead of starting new.
	prompts := agents.SentPrompts()
	if len(prompts) == 0 {
		t.Fatal("expected SendPrompt to be called for reuse_agent")
	}
	if prompts[len(prompts)-1].Message == "" {
		t.Fatal("expected non-empty feedback message")
	}

	// Plan agent completes again → wait_human again.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed"})

	// Now approve.
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation after approve, got %q", agents.LastCall().Role)
	}
}

func TestTriageRetry_Success(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage fails twice.
	for range 2 {
		agents.SimulateComplete("t1")
		if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "failed"}); err != nil {
			t.Fatal(err)
		}
	}

	// Should have retried — 3 StartAgent calls total (1 initial + 2 retries).
	triageCalls := 0
	for _, c := range agents.calls {
		if c.Role == "triage" {
			triageCalls++
		}
	}
	if triageCalls != 3 {
		t.Fatalf("expected 3 triage calls, got %d", triageCalls)
	}

	// Third attempt succeeds.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	// Should advance to set_in_progress → implement.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Fatalf("expected in-progress, got %q", ti.Status)
	}
}

func TestTriageRetry_Exhausted(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Fail 4 times (initial + 3 retries = 4 total, exceeds max_retries: 3).
	for range 4 {
		agents.SimulateComplete("t1")
		// Ignore errors — last one may fail transition resolution.
		_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "failed"})
	}

	// After exhaustion, the transition should resolve (fallback goto: set_in_progress).
	ti, _ := tasks.GetTask("t1")
	// Workflow should have advanced past triage or failed.
	if ti.Workflow.CurrentStep == "triage" && ti.Workflow.State == ExecRunning {
		t.Fatal("expected workflow to advance past triage after retry exhaustion")
	}
}

func TestMatchWorkflow_ReviewTag(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Wrong event type.
	normal := TaskInfo{ID: "t1"}
	if def := engine.MatchWorkflow(normal, "pr.event"); def != nil {
		t.Fatalf("expected no match for pr.event, got %s", def.ID)
	}
}

// addPREventWorkflow writes a minimal pr.event triggered workflow definition
// to the store with the given id, priority, and trigger value. All generated
// workflows share the same single run_agent step that reads its prompt from
// the "prompt" variable — enough to exercise dispatch and variable plumbing.
func addPREventWorkflow(t *testing.T, store *Store, id string, priority int, prIssueKind string) {
	t.Helper()
	def := Definition{
		ID:   id,
		Name: id,
		Trigger: Trigger{
			On:       "pr.event",
			Priority: priority,
			Conditions: []Condition{
				{Field: "pr.issue_kind", Operator: "equals", Value: prIssueKind},
			},
		},
		Steps: []Step{
			{
				ID:   "fix",
				Name: "Fix",
				Type: StepRunAgent,
				Config: StepConfig{
					Role:   "pr-fix",
					Mode:   "headless",
					Model:  "sonnet",
					Prompt: `{{ getvar .Vars "prompt" }}`,
				},
				Next: []Transition{{GoTo: ""}},
			},
		},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

func TestDispatchEvent_MatchesAndStarts(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	engine := NewEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "simple-task",
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
	engine := NewEngine(store, tasks, agents, discardLogger())

	addPREventWorkflow(t, store, "pr-fix-test", 0, "ci_failure")
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-review",
		Workflow: &Execution{
			WorkflowID:  "simple-task",
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

func TestMatchWorkflow_PriorityTieBreak(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	engine := NewEngine(store, tasks, agents, discardLogger())

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

func TestNoWorkflowField(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"}) // no Workflow

	// Should not panic or error fatally.
	engine.HandleAgentComplete("t1", "agent-1", "result", "stopped")
}

func TestResumeStalled_RunAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Simulate a task stuck at "implement" step with no running agent.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	// Should have started an agent for the implement step.
	if agents.CallCount() != 1 {
		t.Fatalf("expected 1 agent start, got %d", agents.CallCount())
	}
	if agents.LastCall().Role != "implementation" {
		t.Fatalf("expected implementation, got %q", agents.LastCall().Role)
	}
}

func TestResumeStalled_SkipWaitHuman(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Task at wait_human step.
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	// Should NOT start any agent.
	if agents.CallCount() != 0 {
		t.Fatalf("expected 0 agent starts for wait_human, got %d", agents.CallCount())
	}
}

func TestHandleHumanAction_NotWaiting(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	err := engine.HandleHumanAction("t1", "approve", nil)
	if err == nil {
		t.Fatal("expected error for non-waiting task")
	}
}

func TestConcurrentAdvance(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	agents.SimulateComplete("t1")

	// Fire two concurrent AdvanceStep calls.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
		}(i)
	}
	wg.Wait()

	// At least one should succeed, at most one should be skipped (nil error, no-op).
	// The engine's inflight guard prevents double-advance.
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	if successCount == 0 {
		t.Fatal("expected at least one successful advance")
	}
}

func TestStartWorkflow_InvalidWorkflowID(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	err := engine.StartWorkflow("t1", "nonexistent-workflow")
	if err == nil {
		t.Fatal("expected error for invalid workflow ID")
	}
}

func TestAdvanceStep_UnknownStepID(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	agents.SimulateComplete("t1")
	err := engine.AdvanceStep("t1", StepOutput{StepID: "nonexistent-step", Status: "completed"})
	if err == nil {
		t.Fatal("expected error for unknown step ID")
	}
}

func TestAdvanceStep_TaskWithoutWorkflow(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	if err == nil {
		t.Fatal("expected error for task without workflow")
	}
}

func TestResumeStalled_SkipsTaskWithRunningAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})
	// Simulate an agent already running.
	_, _ = agents.StartAgent("t1", "implementation", "headless", "sonnet", "test", "", nil, false)

	initialCalls := agents.CallCount()
	engine.ResumeStalled()

	if agents.CallCount() != initialCalls {
		t.Fatal("ResumeStalled should not start another agent when one is already running")
	}
}

func TestResumeStalled_SkipsCompletedWorkflow(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	now := time.Now().UTC()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "done",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "",
			State:       ExecCompleted,
			CompletedAt: &now,
			Variables:   make(map[string]string),
		},
	})

	engine.ResumeStalled()

	if agents.CallCount() != 0 {
		t.Fatal("ResumeStalled should skip completed workflows")
	}
}

func TestShellStep_ExecutesCommand(t *testing.T) {
	// Test the shell step directly using a simple echo command.
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "shell1",
		Type: StepShell,
		Config: StepConfig{
			Command: "echo hello-from-shell",
		},
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1", Title: "test"},
		Step: *step,
		Vars: make(map[string]string),
	}

	output, err := engine.execShell(step, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if output.Status != "completed" {
		t.Fatalf("expected completed, got %q", output.Status)
	}
	if output.Output != "hello-from-shell\n" {
		t.Fatalf("expected 'hello-from-shell\\n', got %q", output.Output)
	}
}

func TestShellStep_FailingCommandSetsStatusFailed(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "shell1",
		Type: StepShell,
		Config: StepConfig{
			Command: "exit 1",
		},
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	output, err := engine.execShell(step, ctx)
	if err != nil {
		t.Fatal(err) // execShell doesn't return error on command failure
	}
	if output.Status != "failed" {
		t.Fatalf("expected failed, got %q", output.Status)
	}
}

func TestExecRunAgent_DefaultModeAndModel(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	step := &Step{
		ID:   "agent1",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "triage",
			Prompt: "test prompt",
			// Mode and Model intentionally empty.
		},
	}

	wfExec := &Execution{
		WorkflowID: "test-simple",
		State:      ExecRunning,
		Variables:  make(map[string]string),
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: wfExec.Variables,
	}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	call := agents.LastCall()
	if call.Mode != "headless" {
		t.Errorf("expected default mode 'headless', got %q", call.Mode)
	}
	if call.Model != "sonnet" {
		t.Errorf("expected default model 'sonnet', got %q", call.Model)
	}
}

func TestHandleAgentComplete_CompletedWorkflowIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Run through full lifecycle to completion.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed"})
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "evaluate", Status: "completed"})

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("precondition: expected completed, got %q", ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "" {
		t.Fatalf("precondition: expected empty current step after completion, got %q", ti.Workflow.CurrentStep)
	}
	historyBefore := len(ti.Workflow.StepHistory)

	// Another agent complete on an already-completed workflow should not
	// start new agents, mutate step history, or record an error.
	callsBefore := agents.CallCount()
	engine.HandleAgentComplete("t1", "stale-agent", "late result", "stopped")

	if agents.CallCount() != callsBefore {
		t.Error("HandleAgentComplete on completed workflow should not start new agents")
	}

	tiAfter, _ := tasks.GetTask("t1")
	if got := len(tiAfter.Workflow.StepHistory); got != historyBefore {
		t.Errorf("StepHistory len = %d, want %d — stale completion must not append",
			got, historyBefore)
	}
	if tiAfter.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted — stale completion must not mutate state",
			tiAfter.Workflow.State)
	}
	if tiAfter.Workflow.CurrentStep != "" {
		t.Errorf("CurrentStep = %q, want empty — stale completion must not mutate current step",
			tiAfter.Workflow.CurrentStep)
	}
}

// TestAdvanceStep_EmptyStepIDIsNoop covers the direct-call variant: a caller
// that passes an empty StepID (e.g. because t.Workflow.CurrentStep was reset
// to "" by a previous completion) used to error with "step not found in
// workflow", which the agent-complete path would log as ERROR and still
// persist via RecordStep. The guard must return nil and leave state intact.
func TestAdvanceStep_EmptyStepIDIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}
	// Force the workflow into the pathological state observed in prod:
	// state=completed, current_step="" — mirrors what resolveNext leaves
	// behind when a terminal step evaluates to goto: "".
	ti, _ := tasks.GetTask("t1")
	ti.Workflow.State = ExecCompleted
	ti.Workflow.CurrentStep = ""
	if err := tasks.SetWorkflow("t1", ti.Workflow); err != nil {
		t.Fatal(err)
	}
	historyBefore := len(ti.Workflow.StepHistory)

	err := engine.AdvanceStep("t1", StepOutput{StepID: "", Status: "completed"})
	if err != nil {
		t.Errorf("AdvanceStep with empty StepID = %v, want nil (no-op)", err)
	}

	tiAfter, _ := tasks.GetTask("t1")
	if got := len(tiAfter.Workflow.StepHistory); got != historyBefore {
		t.Errorf("StepHistory len = %d, want %d — empty-step advance must not append",
			got, historyBefore)
	}
	if tiAfter.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted", tiAfter.Workflow.State)
	}
}

// TestAdvanceStep_FailedWorkflowIsNoop pins the other terminal state:
// workflows that hit ExecFailed also must refuse further advances.
func TestAdvanceStep_FailedWorkflowIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "triage",
			State:       ExecFailed,
			Variables:   make(map[string]string),
		},
	})

	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	if err != nil {
		t.Errorf("AdvanceStep on failed workflow = %v, want nil (no-op)", err)
	}
	if agents.CallCount() != 0 {
		t.Errorf("agents.CallCount = %d, want 0 — failed workflow must not spawn", agents.CallCount())
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{"under limit", "short", 100, "short"},
		{"at limit", "exact", 5, "exact"},
		{"over limit", "this is too long", 7, "this is\n... (truncated)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.limit)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.limit, got, tt.want)
			}
		})
	}
}

func TestAgentModeTemplate(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Advance past triage to implement step.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	// Implement step should have mode resolved from template.
	if agents.LastCall().Mode != "headless" {
		t.Fatalf("expected headless mode from template, got %q", agents.LastCall().Mode)
	}
}

// --- HandleStatusChange + plan-reuse flow ---
//
// These tests cover the fix for interactive plan agents that never exit on
// their own: the workflow must advance from the run_agent step to the
// wait_human review step when the task status flips to the step's
// declared wait_for_status, and reject must re-enter the plan step via a
// set_status intermediate so the next plan-review transition can fire.

// startPlanReuseAtReviewPlan sets up a test-plan-reuse workflow, starts the
// plan agent, and drives it to the review_plan waiting state by flipping
// the task status to plan-review. Returns the configured engine/mocks for
// further assertions.
func startPlanReuseAtReviewPlan(t *testing.T) (*Engine, *memTasks, *mockAgents) {
	t.Helper()
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	if got := agents.LastCall().Role; got != "plan" {
		t.Fatalf("expected plan agent started, got %q", got)
	}

	// Simulate the plan agent flipping the task status — this is what
	// the agent would do via `synapse-cli update --status plan-review`.
	tasks.SetStatus("t1", "plan-review")
	engine.HandleStatusChange("t1", "plan-review")

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("expected review_plan after status advance, got %q", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("expected ExecWaiting at review_plan, got %q", ti.Workflow.State)
	}
	return engine, tasks, agents
}

func TestHandleStatusChange_AdvancesRunAgentWhenWaitForStatusMatches(t *testing.T) {
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
		t.Fatal(err)
	}

	// Before the status flips, we're still in the plan run_agent step.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Fatalf("precondition: expected plan step, got %q", ti.Workflow.CurrentStep)
	}

	// The plan agent flips the task status — engine should advance to
	// review_plan without the agent process having to exit.
	tasks.SetStatus("t1", "plan-review")
	engine.HandleStatusChange("t1", "plan-review")

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("CurrentStep = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}
}

func TestHandleStatusChange_NoOp(t *testing.T) {
	tests := []struct {
		name      string
		newStatus string
		// mutate lets each case set up its own pre-state after the
		// default "workflow started, sitting in plan step" arrangement.
		mutate func(tasks *memTasks)
	}{
		{
			name:      "status does not match wait_for_status",
			newStatus: "todo",
		},
		{
			name:      "current step is not a run_agent",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow.CurrentStep = "review_plan"
				_ = tasks.SetWorkflow("t1", ti.Workflow)
			},
		},
		{
			name:      "workflow already completed",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow.State = ExecCompleted
				_ = tasks.SetWorkflow("t1", ti.Workflow)
			},
		},
		{
			name:      "task has no workflow",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow = nil
				tasks.Put(ti)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStoreWith(t, "test-plan-reuse.yaml")
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
			if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
				t.Fatal(err)
			}

			if tt.mutate != nil {
				tt.mutate(tasks)
			}

			// Snapshot the current step so we can detect any advance.
			before, _ := tasks.GetTask("t1")
			wantStep := ""
			if before.Workflow != nil {
				wantStep = before.Workflow.CurrentStep
			}

			engine.HandleStatusChange("t1", tt.newStatus)

			after, _ := tasks.GetTask("t1")
			gotStep := ""
			if after.Workflow != nil {
				gotStep = after.Workflow.CurrentStep
			}
			if gotStep != wantStep {
				t.Errorf("CurrentStep changed to %q, want %q (no advance)", gotStep, wantStep)
			}
		})
	}
}

func TestHandleStatusChange_UnknownTaskDoesNotPanic(t *testing.T) {
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Act — must not panic even though the task was never registered.
	engine.HandleStatusChange("ghost", "plan-review")
}

func TestPlanReuse_RejectResetsStatusAndReusesAgentWithFeedback(t *testing.T) {
	engine, tasks, agents := startPlanReuseAtReviewPlan(t)

	// Arrange — the plan agent is still "running" (reuse_agent relies on
	// FindRunningAgentForRole). Record how many SendPrompt calls we've
	// seen so we can assert exactly one more is added by the reject.
	sentBefore := len(agents.SentPrompts())

	// Act — user rejects the plan with free-text feedback. The reject
	// branch routes review_plan → start_replan (set_status planning) →
	// plan, which hits the reuse_agent path.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "add error handling"}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// Assert 1 — task status was reset by start_replan, so the next
	// plan-review transition is observable as a real change event.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "planning" {
		t.Errorf("Status = %q, want planning (reset by start_replan)", ti.Status)
	}

	// Assert 2 — the workflow re-entered the plan run_agent step.
	if ti.Workflow.CurrentStep != "plan" {
		t.Errorf("CurrentStep = %q, want plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}

	// Assert 3 — the reused agent received exactly one new prompt
	// carrying the feedback (verbatim, via the rendered template).
	sent := agents.SentPrompts()
	if len(sent) != sentBefore+1 {
		t.Fatalf("SendPrompt count = %d, want %d", len(sent), sentBefore+1)
	}
	msg := sent[len(sent)-1].Message
	if !strings.Contains(msg, "Plan rejected") {
		t.Errorf("prompt missing rejection header: %q", msg)
	}
	if !strings.Contains(msg, "add error handling") {
		t.Errorf("prompt missing feedback: %q", msg)
	}

	// Assert 4 — no new agent was spawned (reuse, not restart).
	if got := agents.CallCount(); got != 1 {
		t.Errorf("StartAgent called %d times, want 1 (reuse only)", got)
	}
}

func TestPlanReuse_RejectThenReplanAdvancesOnStatusChange(t *testing.T) {
	engine, tasks, _ := startPlanReuseAtReviewPlan(t)

	// Reject — workflow should re-enter plan step waiting for the agent.
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "needs detail"}); err != nil {
		t.Fatal(err)
	}

	// Simulate the plan agent delivering a revised plan and flipping
	// the status back to plan-review.
	tasks.SetStatus("t1", "plan-review")
	engine.HandleStatusChange("t1", "plan-review")

	// The workflow should be back at review_plan waiting for a fresh
	// human action. Without the set_status reset, the status would
	// already be plan-review when the agent ran and no hook would fire.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("CurrentStep = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}
}

func TestPlanReuse_ApproveAdvancesPastReviewPlan(t *testing.T) {
	engine, tasks, _ := startPlanReuseAtReviewPlan(t)

	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("Status = %q, want in-progress (set by done step)", ti.Status)
	}
	if ti.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted", ti.Workflow.State)
	}
}
