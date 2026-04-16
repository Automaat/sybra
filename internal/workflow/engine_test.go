package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Test helpers ---

func init() {
	// Skip real backoff waits in the ensure_pr_closes_issue verify
	// retry loop — tests drive attempt counts via the linker queue.
	prVerifySleep = func(time.Duration) {}
	prVerifyBackoffs = []time.Duration{0, 0, 0}
}

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
	mu      sync.Mutex
	tasks   map[string]*TaskInfo
	reasons map[string]string
}

func newMemTasks() *memTasks {
	return &memTasks{tasks: make(map[string]*TaskInfo), reasons: make(map[string]string)}
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

func (m *memTasks) UpdateTaskStatus(id, status, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.Status = status
	m.reasons[id] = reason
	return nil
}

// Reason returns the last status reason recorded for a task.
func (m *memTasks) Reason(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reasons[id]
}

func (m *memTasks) UpdateTaskPR(id string, prNumber int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.PRNumber = prNumber
	return nil
}

func (m *memTasks) MarkTaskReviewed(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("task %s not found", id)
	}
	t.Reviewed = true
	m.tasks[id] = t
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
	TaskID, Role, Mode, Model, Provider, Prompt, Dir string
	AllowedTools                                     []string
	NeedsWorktree                                    bool
	OneShot                                          bool
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

func (m *mockAgents) StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	id := fmt.Sprintf("agent-%d", m.counter)
	m.calls = append(m.calls, startCall{
		TaskID: taskID, Role: role, Mode: mode, Model: model, Provider: provider,
		Prompt: prompt, Dir: dir, AllowedTools: allowedTools,
		NeedsWorktree: needsWorktree, OneShot: oneShot,
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

func (m *mockAgents) DefaultProvider() string { return "claude" }

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

	// Simulate implement completes. The mechanical evaluate step runs
	// inline during AdvanceStep and terminates the workflow without
	// spawning a new agent.
	implCallCount := agents.CallCount()
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed", Output: "Done.", AgentID: "agent-impl"}); err != nil {
		t.Fatal(err)
	}

	if got := agents.CallCount(); got != implCallCount {
		t.Errorf("evaluate spawned an agent (calls before=%d, after=%d) — should be mechanical", implCallCount, got)
	}

	// Workflow should be completed; mechanical evaluate flips to human-required.
	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("expected completed, got %q (current step %q)", ti.Workflow.State, ti.Workflow.CurrentStep)
	}
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

// TestOneShot_ComputedFromStepConfig verifies that the engine asks the launcher
// for a one-shot run exactly when an interactive step has no reuse_agent and
// no wait_for_status. Without this flag interactive conversational agents sit
// in StatePaused forever and the workflow can never reach the evaluator.
func TestOneShot_ComputedFromStepConfig(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Interactive-mode task forces the templated implement step into
	// interactive mode via {{.Task.AgentMode}}.
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage is headless → never one-shot.
	triageCall := agents.LastCall()
	if triageCall.Role != "triage" {
		t.Fatalf("expected triage, got %q", triageCall.Role)
	}
	if triageCall.OneShot {
		t.Errorf("triage (headless) should not be one-shot")
	}

	// Advance through triage → planning so plan fires.
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "plan please"}); err != nil {
		t.Fatal(err)
	}

	// Plan step is interactive + reuse_agent=true → must NOT be one-shot,
	// otherwise the agent dies between turns and plan-review replanning breaks.
	planCall := agents.LastCall()
	if planCall.Role != "plan" {
		t.Fatalf("expected plan, got %q", planCall.Role)
	}
	if planCall.Mode != "interactive" {
		t.Fatalf("plan mode = %q, want interactive", planCall.Mode)
	}
	if planCall.OneShot {
		t.Errorf("plan step has reuse_agent=true — must not be one-shot")
	}

	// Approve plan → set_in_progress → implement. The implement step resolves
	// to interactive via the task's AgentMode. No reuse_agent, no
	// wait_for_status → this is the case that needs OneShot=true.
	tasks.SetStatus("t1", "plan-review")
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan", Status: "completed", Output: "plan ready"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatal(err)
	}

	implCall := agents.LastCall()
	if implCall.Role != "implementation" {
		t.Fatalf("expected implementation, got %q", implCall.Role)
	}
	if implCall.Mode != "interactive" {
		t.Fatalf("impl mode = %q, want interactive", implCall.Mode)
	}
	if !implCall.OneShot {
		t.Errorf("interactive implement without reuse_agent / wait_for_status must be one-shot so the agent exits and evaluate can run")
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
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Result: "result", Success: true})
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
		wg.Go(func() {
			errs[i] = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
		})
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

	// An advance for a step that does not match the workflow's current step
	// is a stale completion (e.g. a duplicate agent from a ResumeStalled race,
	// or a stray callback after the workflow advanced). The engine must
	// silently no-op instead of crashing or mutating step history — that
	// guard is what stops a second plan agent from driving review_plan into
	// ExecFailed when its delayed completion arrives after the human gate.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "nonexistent-step", Status: "completed"}); err != nil {
		t.Fatalf("stale stepID should be a no-op, got err: %v", err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "triage" {
		t.Errorf("CurrentStep = %q, want unchanged triage", ti.Workflow.CurrentStep)
	}
	if got := len(ti.Workflow.StepHistory); got != 0 {
		t.Errorf("step history len = %d, want 0 — stale advance must not append", got)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("state = %q, want ExecWaiting (unchanged)", ti.Workflow.State)
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
	_, _ = agents.StartAgent("t1", "implementation", "headless", "sonnet", "", "test", "", nil, false, false)

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

// TestShellStep_StdinReaderExitsOnEOF covers a subtle deadlock: execShell
// does not wire stdin, so commands that call `read` or `cat` inherit a
// nil/closed stdin and should exit immediately with EOF. A regression that
// passed through os.Stdin (or left the pipe dangling) would cause the shell
// step to hang for the full shellTimeout (30s). The 5-second deadline here
// proves the command exits promptly.
func TestShellStep_StdinReaderExitsOnEOF(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "stdin-reader",
		Type: StepShell,
		Config: StepConfig{
			// `read` exits non-zero on EOF. `cat` exits 0 immediately since
			// its stdin is empty. Both should be fast.
			Command: "cat",
		},
	}
	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	done := make(chan error, 1)
	go func() {
		_, err := engine.execShell(step, ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execShell: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execShell hung on stdin-reading command — sybra provides no stdin, `cat` should EOF immediately")
	}
}

// TestShellStep_ContextCancelKillsCommand verifies that cancelling the
// engine's parent context terminates a long-running shell step promptly
// rather than waiting out the 30s shellTimeout. execShell derives its own
// context via context.WithTimeout(e.ctx, shellTimeout); cancelling e.ctx
// must propagate down and kill the subprocess via exec.CommandContext.
// A regression that used context.Background() instead of e.ctx would
// leave the command running after app shutdown.
func TestShellStep_ContextCancelKillsCommand(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	parentCtx, cancel := context.WithCancel(context.Background())
	engine.SetContext(parentCtx)

	step := &Step{
		ID:   "long-sleep",
		Type: StepShell,
		Config: StepConfig{
			Command: "sleep 30",
		},
	}
	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	// Cancel after 200ms; the sleep would otherwise run 30 seconds.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	output, err := engine.execShell(step, ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("execShell: %v", err)
	}
	// Killed subprocess is "failed", not "completed".
	if output.Status != "failed" {
		t.Errorf("status = %q, want failed (subprocess killed by ctx cancel)", output.Status)
	}
	// Must return within a handful of seconds — certainly well under 30s
	// shellTimeout. 10s is plenty of slack for slow CI.
	if elapsed > 10*time.Second {
		t.Errorf("execShell took %v after ctx cancel — should return promptly", elapsed)
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
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "stale-agent", Result: "late result", Success: true})

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
	// the agent would do via `sybra-cli update --status plan-review`.
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

// --- ensure_pr_closes_issue step ---

// fakePRLinker is a scripted PRLinker used by executor tests.
type fakePRLinker struct {
	// getQueue yields successive GetClosingIssues results.
	getQueue []getResult
	getCalls int

	editErr   error
	editCalls int
	lastBody  string
}

type getResult struct {
	issues []int
	body   string
	err    error
}

func (f *fakePRLinker) GetClosingIssues(_ string, _ int) (issues []int, body string, err error) {
	idx := f.getCalls
	f.getCalls++
	if idx >= len(f.getQueue) {
		idx = len(f.getQueue) - 1
	}
	r := f.getQueue[idx]
	return r.issues, r.body, r.err
}

func (f *fakePRLinker) EditBody(_ string, _ int, body string) error {
	f.editCalls++
	f.lastBody = body
	return f.editErr
}

func newEnsurePRStep() *Step {
	return &Step{ID: "ensure", Type: StepEnsurePRClosesIssue}
}

func TestExecEnsurePRClosesIssue_NoLinkerSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5, Issue: "https://github.com/owner/repo/issues/7"}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "no pr linker") {
		t.Errorf("Output = %q, want 'no pr linker' skip reason", out.Output)
	}
}

func TestExecEnsurePRClosesIssue_MissingFieldsSkip(t *testing.T) {
	tests := []struct {
		name string
		ti   TaskInfo
	}{
		{"no issue", TaskInfo{ID: "t1", ProjectID: "owner/repo", PRNumber: 5}},
		{"no pr", TaskInfo{ID: "t1", ProjectID: "owner/repo", Issue: "https://github.com/owner/repo/issues/7"}},
		{"no project", TaskInfo{ID: "t1", PRNumber: 5, Issue: "https://github.com/owner/repo/issues/7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewEngine(store, tasks, agents, discardLogger())
			engine.SetPRLinker(&fakePRLinker{})

			out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), tt.ti)
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "completed" {
				t.Errorf("Status = %q, want completed", out.Status)
			}
			if !strings.Contains(out.Output, "skipped") {
				t.Errorf("Output = %q, want 'skipped' reason", out.Output)
			}
		})
	}
}

func TestExecEnsurePRClosesIssue_CrossRepoSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{}
	engine.SetPRLinker(linker)

	ti := TaskInfo{
		ID:        "t1",
		ProjectID: "owner/repo",
		PRNumber:  5,
		Issue:     "https://github.com/other/elsewhere/issues/7",
	}
	out, _ := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "cross-repo") {
		t.Errorf("Output = %q, want cross-repo skip", out.Output)
	}
	if linker.getCalls != 0 {
		t.Errorf("GetClosingIssues called %d times, want 0 (skip before fetch)", linker.getCalls)
	}
}

func TestExecEnsurePRClosesIssue_AlreadyLinkedNoEdit(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{issues: []int{7}, body: "original"}},
	}
	engine.SetPRLinker(linker)

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "already linked") {
		t.Errorf("output = %+v, want completed/already linked", out)
	}
	if linker.editCalls != 0 {
		t.Errorf("EditBody called %d times, want 0", linker.editCalls)
	}
}

func TestExecEnsurePRClosesIssue_EditAppendsAndVerifies(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			{issues: nil, body: "Implements the feature."},
			{issues: []int{7}, body: "Implements the feature.\n\nCloses https://github.com/owner/repo/issues/7"},
		},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if linker.editCalls != 1 {
		t.Errorf("EditBody called %d times, want 1", linker.editCalls)
	}
	wantBody := "Implements the feature.\n\nCloses https://github.com/owner/repo/issues/7"
	if linker.lastBody != wantBody {
		t.Errorf("edit body = %q, want %q", linker.lastBody, wantBody)
	}
	// Status must not have been changed on success.
	after, _ := tasks.GetTask("t1")
	if after.Status != "in-review" {
		t.Errorf("Status = %q, want in-review (unchanged)", after.Status)
	}
}

func TestExecEnsurePRClosesIssue_EmptyBodyNoLeadingNewlines(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			{issues: nil, body: ""},
			{issues: []int{7}, body: "Closes https://github.com/owner/repo/issues/7"},
		},
	}
	engine.SetPRLinker(linker)

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	if _, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti); err != nil {
		t.Fatal(err)
	}
	if linker.lastBody != "Closes https://github.com/owner/repo/issues/7" {
		t.Errorf("edit body = %q, want no leading newlines", linker.lastBody)
	}
}

func TestExecEnsurePRClosesIssue_EditFailureFlipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{issues: nil, body: "body"}},
		editErr:  fmt.Errorf("403 forbidden"),
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" {
		t.Errorf("Status = %q, want failed", out.Status)
	}
	after, _ := tasks.GetTask("t1")
	if after.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", after.Status)
	}
}

// Verification lag is a false negative: gh pr edit succeeded, the
// body contains "Closes <url>", but GitHub hasn't re-parsed
// closingIssuesReferences yet. The step must trust the body and
// leave the task status alone instead of flipping to human-required.
func TestExecEnsurePRClosesIssue_VerifyLagTrustsBody(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			// 1 pre-check + 4 verify attempts, all miss.
			{issues: nil, body: "body"},
			{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
			{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
			{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
			{issues: nil, body: "body\n\nCloses https://github.com/owner/repo/issues/7"},
		},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed (verification lag is soft-fail)", out.Status)
	}
	if !strings.Contains(out.Output, "trusting body") {
		t.Errorf("Output = %q, want 'trusting body' message", out.Output)
	}
	after, _ := tasks.GetTask("t1")
	if after.Status != "in-review" {
		t.Errorf("task status = %q, want in-review (unchanged)", after.Status)
	}
	// 1 pre-check + 1 initial verify + 3 retries = 5 fetches.
	if linker.getCalls != 5 {
		t.Errorf("GetClosingIssues calls = %d, want 5 (pre-check + 4 verify attempts)", linker.getCalls)
	}
}

// Verification should retry: first post-edit fetch misses (GitHub
// lagging), second fetch sees the parsed closing reference.
func TestExecEnsurePRClosesIssue_VerifyRetrySucceeds(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			{issues: nil, body: "body"},                    // pre-check miss → triggers edit
			{issues: nil, body: "body\n\nCloses ..."},      // verify attempt 0: still stale
			{issues: []int{7}, body: "body\n\nCloses ..."}, // verify attempt 1: parsed
		},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "linked issue #7") {
		t.Errorf("out = %+v, want completed/linked issue #7", out)
	}
	if linker.getCalls != 3 {
		t.Errorf("GetClosingIssues calls = %d, want 3 (pre-check + 2 verify attempts)", linker.getCalls)
	}
}

// Verification fetch that errors on every retry is still a soft-fail:
// the edit went through, so trust the body we wrote.
func TestExecEnsurePRClosesIssue_VerifyErrorTrustsBody(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{
			{issues: nil, body: "body"},
			{err: errors.New("network timeout")},
			{err: errors.New("network timeout")},
			{err: errors.New("network timeout")},
			{err: errors.New("network timeout")},
		},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "trusting body") {
		t.Errorf("Output = %q, want 'trusting body' message", out.Output)
	}
	after, _ := tasks.GetTask("t1")
	if after.Status != "in-review" {
		t.Errorf("task status = %q, want in-review (unchanged)", after.Status)
	}
}

func TestExecEnsurePRClosesIssue_FetchErrorIsSoftFail(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	linker := &fakePRLinker{
		getQueue: []getResult{{err: errors.New("network timeout")}},
	}
	engine.SetPRLinker(linker)

	tasks.Put(TaskInfo{ID: "t1", Status: "in-review"})

	ti := TaskInfo{
		ID: "t1", ProjectID: "owner/repo", PRNumber: 5,
		Issue: "https://github.com/owner/repo/issues/7",
	}
	out, err := engine.execEnsurePRClosesIssue("t1", newEnsurePRStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	// Fetch failure must not block the workflow or flip status.
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed (fetch errors are soft-fail)", out.Status)
	}
	after, _ := tasks.GetTask("t1")
	if after.Status != "in-review" {
		t.Errorf("task status = %q, want in-review (unchanged)", after.Status)
	}
}

// TestDuplicatePlanAgent_StaleCompletionDoesNotFailWaitHuman reproduces the
// production bug that left task 5a5ad276 stuck: a ResumeStalled race spawned
// two plan agents; the first completed and advanced plan → review_plan
// (wait_human); the second completed seconds later and the engine credited
// its completion to the current step (review_plan), ran resolveNext with no
// human_action var set, failed to match any transition, and set state to
// ExecFailed. HandleHumanAction then refused the user's reject click with
// "task X is not waiting for human action".
//
// The fix: HandleAgentComplete uses the step the agent was actually spawned
// for (tracked in engine.agentSteps), and AdvanceStep drops completions whose
// StepID doesn't match the workflow's current step.
func TestDuplicatePlanAgent_StaleCompletionDoesNotFailWaitHuman(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Triage runs → agent flips status to planning → advance into plan step.
	triageAgent := agents.LastID()
	tasks.SetStatus("t1", "planning")
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: triageAgent, Result: "triaged", Success: true})

	planAgent1 := agents.LastID()
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Fatalf("precondition: current_step = %q, want plan", ti.Workflow.CurrentStep)
	}

	// Inject a duplicate plan agent as if a ResumeStalled ticker fired
	// during the interactive-spawn window and raced the first agent. The
	// engine.agentSteps mapping records what execRunAgent would have set.
	agents.mu.Lock()
	agents.counter++
	planAgent2 := fmt.Sprintf("agent-%d", agents.counter)
	agents.calls = append(agents.calls, startCall{TaskID: "t1", Role: "plan", Mode: "interactive"})
	agents.running["t1"] = planAgent2
	agents.roles["t1/plan"] = planAgent2
	agents.mu.Unlock()
	engine.mu.Lock()
	engine.agentSteps[planAgent2] = "plan"
	engine.mu.Unlock()

	// Agent 1 completes first → workflow advances to review_plan/wait_human.
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: planAgent1, Result: "plan ready", Success: true})

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("after first plan completion: current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Fatalf("after first plan completion: state = %q, want ExecWaiting", ti.Workflow.State)
	}

	// Agent 2 (the duplicate) finishes seconds later. Old behavior would
	// drive review_plan into ExecFailed. New behavior: dropped as stale.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: planAgent2, Result: "plan ready", Success: true})

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("after stale completion: state = %q, want ExecWaiting", ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("after stale completion: current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}

	// The human's rejection must now succeed — this is the end-to-end
	// symptom the user reported ("task is not waiting for human action").
	if err := engine.HandleHumanAction("t1", "reject", map[string]string{"feedback": "try again"}); err != nil {
		t.Fatalf("HandleHumanAction reject after stale duplicate: %v", err)
	}

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Errorf("after reject: current_step = %q, want plan (loop back)", ti.Workflow.CurrentStep)
	}
}

// TestHandleAgentComplete_WaitHumanWithoutActionIsNoop is the defense-in-depth
// guard for the same bug. If a stray agent completion slips past the stale-
// step check and lands on a wait_human step without a human_action var set
// (e.g. an untracked legacy agent where HandleAgentComplete falls back to
// CurrentStep), AdvanceStep must still refuse to run resolveNext. Otherwise
// the workflow would fail on an unmatched transition and permanently seal
// the human review gate.
func TestHandleAgentComplete_WaitHumanWithoutActionIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Put a task directly at the wait_human step with no agent tracked.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "plan-review",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// Agent callback arrives for the current (wait_human) step with no
	// human_action set. Must be a no-op.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "untracked-legacy-agent", Result: "unexpected result", Success: true})

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("state = %q, want ExecWaiting — stray completion on wait_human must not fail the workflow",
			ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if got := len(ti.Workflow.StepHistory); got != 0 {
		t.Errorf("step_history len = %d, want 0 — stray wait_human completion must not append", got)
	}

	// Rejection still works after the defense kicks in.
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatalf("HandleHumanAction approve: %v", err)
	}
}

// TestResumeStalled_SkipsInflightDispatch exercises the ResumeStalled → race
// that actually produced the duplicate spawn in prod. The ResumeStalled
// ticker fires during the 1-3s window while an interactive plan step is
// still preparing its worktree and starting the claude process — at that
// point no agent is registered yet so HasRunningAgent returns false.
// Without the inflight guard the ticker would call executeSteps → execRunAgent
// and spawn a second agent for the same step. With the guard it must skip.
func TestResumeStalled_SkipsInflightDispatch(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Task sitting at an interactive run_agent step with no running agent —
	// the shape ResumeStalled normally resumes.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "planning",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// Simulate the original dispatch being mid-flight inside AdvanceStep —
	// inflight[t1] is set, no agent registered yet (worktree still being
	// created in the real system, fake-claude hasn't started).
	engine.mu.Lock()
	engine.inflight["t1"] = struct{}{}
	engine.mu.Unlock()

	before := agents.CallCount()
	engine.ResumeStalled()
	if got := agents.CallCount(); got != before {
		t.Errorf("ResumeStalled spawned a duplicate agent: calls %d → %d (expected no change while inflight)",
			before, got)
	}

	// Once the original dispatch finishes and clears inflight, a subsequent
	// tick is allowed to resume — that's the real recovery path.
	engine.mu.Lock()
	delete(engine.inflight, "t1")
	engine.mu.Unlock()

	engine.ResumeStalled()
	if got := agents.CallCount(); got != before+1 {
		t.Errorf("ResumeStalled after inflight cleared: calls %d → %d (want +1)", before, got)
	}
}

// TestExecRunAgent_TracksSpawnedStep verifies that execRunAgent populates
// the engine.agentSteps map so HandleAgentComplete can route completions
// back to the right step. Without this mapping, a delayed completion from
// a duplicate agent would be credited to whatever CurrentStep happens to
// be at the moment — the exact bug that corrupted review_plan.
func TestExecRunAgent_TracksSpawnedStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})

	step := &Step{
		ID:   "plan",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "plan",
			Mode:   "interactive",
			Prompt: "p",
		},
	}
	wfExec := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	agentID := agents.LastID()
	engine.mu.Lock()
	gotStep, tracked := engine.agentSteps[agentID]
	engine.mu.Unlock()
	if !tracked {
		t.Fatalf("agentSteps missing entry for agent %s", agentID)
	}
	if gotStep != "plan" {
		t.Errorf("agentSteps[%s] = %q, want plan", agentID, gotStep)
	}

	// Completing the agent must clear its mapping so the map doesn't grow
	// unbounded across long-lived sessions.
	tasks.SetStatus("t1", "plan-review")
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: agentID, Result: "done", Success: true})

	engine.mu.Lock()
	_, stillThere := engine.agentSteps[agentID]
	engine.mu.Unlock()
	if stillThere {
		t.Errorf("agentSteps still has %s after completion — mapping leaked", agentID)
	}
}

func TestExecuteSteps_CycleDetection(t *testing.T) {
	store := newTestStoreWith(t, "test-cycle.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "cycle1", Status: "todo", AgentMode: "headless"})

	err := engine.StartWorkflow("cycle1", "test-cycle")
	if err == nil {
		t.Fatal("expected error for cyclic workflow, got nil")
	}

	var cycleErr *CycleError
	if !errors.As(err, &cycleErr) {
		t.Fatalf("expected *CycleError, got %T: %v", err, err)
	}
	if cycleErr.StepID == "" {
		t.Error("CycleError.StepID is empty")
	}
	if cycleErr.At <= cycleErr.FirstAt {
		t.Errorf("CycleError.At (%d) should be > FirstAt (%d)", cycleErr.At, cycleErr.FirstAt)
	}
}

func TestParseIssueURL(t *testing.T) {
	tests := []struct {
		url      string
		wantRepo string
		wantNum  int
	}{
		{"https://github.com/owner/repo/issues/42", "owner/repo", 42},
		{"https://github.com/owner/repo/pull/42", "", 0},
		{"https://github.com/owner/repo/issues/abc", "", 0},
		{"https://github.com/owner/repo/issues/0", "", 0},
		{"not a url", "", 0},
		{"", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			gotRepo, gotNum := parseIssueURL(tt.url)
			if gotRepo != tt.wantRepo || gotNum != tt.wantNum {
				t.Errorf("parseIssueURL(%q) = %q,%d; want %q,%d",
					tt.url, gotRepo, gotNum, tt.wantRepo, tt.wantNum)
			}
		})
	}
}

// --- verify_commits step ---

// fakeWorktreeGetter is a scripted WorktreeGetter for tests.
type fakeWorktreeGetter struct {
	path string
	ok   bool
}

func (f *fakeWorktreeGetter) GetWorktreePath(_ string) (string, bool) {
	return f.path, f.ok
}

func newVerifyCommitsStep() *Step {
	return &Step{ID: "verify", Type: StepVerifyCommits}
}

// makeGitRepo creates a bare-minimum git repo with an initial commit on main
// and optionally an extra commit on the current HEAD (simulating a task branch
// that is ahead of origin/main).
//
// Returns the worktree directory path.
func makeGitRepo(t *testing.T, withExtraCommit bool) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")

	// Create initial commit so origin/main exists.
	f := filepath.Join(dir, "README.md")
	if err := os.WriteFile(f, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "init")

	// Teach git about a local "remote" so origin/main resolves.
	// We use the repo itself as its own origin (bare clone not needed for tests).
	run("remote", "add", "origin", dir)
	run("fetch", "origin")

	if withExtraCommit {
		f2 := filepath.Join(dir, "change.txt")
		if err := os.WriteFile(f2, []byte("change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "change.txt")
		run("commit", "-m", "feat: task work")
	}

	return dir
}

func TestExecVerifyCommits_NoGetterSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	// worktrees nil by default

	ti := TaskInfo{ID: "t1"}
	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "skipped") {
		t.Errorf("Output = %q, want skipped", out.Output)
	}
}

func TestExecVerifyCommits_NoWorktreeSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{ok: false})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "skipped") {
		t.Errorf("Output = %q, want skipped", out.Output)
	}
}

func TestExecVerifyCommits_WithCommitsVerified(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, true /* withExtraCommit */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Errorf("Output = %q, want 'commits verified'", out.Output)
	}
	// Task status must not change.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress", ti.Status)
	}
}

func TestExecVerifyCommits_GitErrorFlipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	// Path exists but is not a git repo — git log returns non-zero,
	// simulating the broken-worktree scenario from the synapse→sybra rename.
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "git error") {
		t.Errorf("Output = %q, want 'git error'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "worktree git error") {
		t.Errorf("status reason = %q, want 'worktree git error'", reason)
	}
}

func TestExecVerifyCommits_NoCommitsFlipsHumanRequired(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "no commits") {
		t.Errorf("Output = %q, want 'no commits'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

// --- evaluate step ---

func newEvaluateStep() *Step {
	return &Step{ID: "evaluate", Type: StepEvaluate}
}

func newEngineForEval(t *testing.T, tasks *memTasks) *Engine {
	t.Helper()
	store := newTestStore(t)
	agents := newMockAgents()
	return NewEngine(store, tasks, agents, discardLogger())
}

func TestExecEvaluate_LastAgentFailedFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "rate limit exceeded"},
		},
	}

	out, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("step Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if got := tasks.Reason("t1"); got != "rate limit exceeded" {
		t.Errorf("reason = %q, want %q", got, "rate limit exceeded")
	}
}

func TestExecEvaluate_LastAgentFailedTruncatesLongReason(t *testing.T) {
	long := strings.Repeat("x", 500)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: long},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	got := tasks.Reason("t1")
	if !strings.Contains(got, "(truncated)") {
		t.Errorf("reason missing truncation marker: %q", got)
	}
	if len(got) >= len(long) {
		t.Errorf("reason not truncated: %d chars", len(got))
	}
}

func TestExecEvaluate_LastAgentSucceededFlipsHumanRequiredWithDefault(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1", Output: "Implementation done."},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if got := tasks.Reason("t1"); got != "commits pushed but no PR created" {
		t.Errorf("reason = %q, want %q", got, "commits pushed but no PR created")
	}
}

func TestExecEvaluate_SkipsMechanicalStepsInHistory(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "real error"},
			{StepID: "verify_commits", Status: "completed"},
			{StepID: "link_pr_and_review", Status: "completed", Output: "no pr found"},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	if got := tasks.Reason("t1"); got != "real error" {
		t.Errorf("reason = %q, want %q (mechanical steps must be skipped)", got, "real error")
	}
}

func TestExecEvaluate_EmptyHistory(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if got := tasks.Reason("t1"); got != "no agent result to evaluate" {
		t.Errorf("reason = %q, want %q", got, "no agent result to evaluate")
	}
}

func TestExecEvaluate_FailedWithEmptyOutput(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "   "},
		},
	}

	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, TaskInfo{}); err != nil {
		t.Fatal(err)
	}
	if got := tasks.Reason("t1"); got != "agent failed with no output" {
		t.Errorf("reason = %q, want %q", got, "agent failed with no output")
	}
}

func TestExecEvaluate_NoPRFallsThrough(t *testing.T) {
	// When ProjectID+Branch are set but gh pr list finds nothing, the step must
	// still fall through to human-required (not panic or error).
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "owner/repo", Branch: "feature-branch"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "failed", AgentID: "a1", Output: "timed out"},
		},
	}

	ti := TaskInfo{ID: "t1", ProjectID: "owner/repo", Branch: "feature-branch"}
	if _, err := engine.execEvaluate("t1", newEvaluateStep(), wfExec, ti); err != nil {
		t.Fatal(err)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}

func newLinkPRStep() *Step {
	return &Step{ID: "link_pr_and_review", Type: StepLinkPRAndReview}
}

func TestExecLinkPRAndReview_PRAlreadyLinked(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 42})
	engine := newEngineForEval(t, tasks)

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), &Execution{}, TaskInfo{ID: "t1", PRNumber: 42})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 42 {
		t.Errorf("pr_number = %d, want 42", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_FullURLInAgentOutput(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1",
				Output: "PR created: https://github.com/owner/repo/pull/123"},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 123 {
		t.Errorf("pr_number = %d, want 123", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_ShortRefInAgentOutput(t *testing.T) {
	// Agents sometimes output "owner/repo#N" instead of a full GitHub URL.
	// The step must parse this shorthand and link the PR.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1",
				Output: "PR created: Automaat/sybra#444\n\nChanges applied."},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-review" {
		t.Errorf("task status = %q, want in-review", ti.Status)
	}
	if ti.PRNumber != 444 {
		t.Errorf("pr_number = %d, want 444", ti.PRNumber)
	}
}

func TestExecLinkPRAndReview_NoPRFallsThrough(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	wfExec := &Execution{
		StepHistory: []StepRecord{
			{StepID: "implement", Status: "completed", AgentID: "a1", Output: "changes pushed"},
		},
	}

	out, err := engine.execLinkPRAndReview("t1", newLinkPRStep(), wfExec, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (must not change)", ti.Status)
	}
}

func TestAdvanceStep_MarkReviewedAfterReviewRole(t *testing.T) {
	// After a run_agent step with role=review completes successfully,
	// the task must be marked reviewed so re-triggered workflows skip code_review.
	store := newTestStoreWith(t, "test-review-fix.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-review-fix"); err != nil {
		t.Fatal(err)
	}

	// implement → maybe_review → code_review
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed", AgentID: "a1"}); err != nil {
		t.Fatal(err)
	}
	// Workflow should now be waiting at code_review.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "code_review" {
		t.Fatalf("expected code_review step, got %q", ti.Workflow.CurrentStep)
	}

	// Complete code_review (role=review) → must mark reviewed.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "code_review", Status: "completed", AgentID: "a2", Output: "review done"}); err != nil {
		t.Fatal(err)
	}

	ti, _ = tasks.GetTask("t1")
	if !ti.Reviewed {
		t.Error("task.Reviewed = false after review-role step completed; want true")
	}
}

// TestAdvanceStep_WorkflowDefinitionDeletedMidRun covers the case where a
// workflow YAML file is removed from disk while an execution is in flight.
// loadAdvanceContext re-reads the definition from the store for every
// AdvanceStep call, so a deleted file must surface a clear error instead of
// panicking or silently reusing stale state. The task's workflow reference
// stays put — the caller decides whether to reset it.
func TestAdvanceStep_WorkflowDefinitionDeletedMidRun(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// The definition file disappears (user-edit, git clean, rm -rf).
	if err := store.Delete("test-simple"); err != nil {
		t.Fatalf("Delete definition: %v", err)
	}

	agents.SimulateComplete("t1")
	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "triaged"})
	if err == nil {
		t.Fatal("AdvanceStep after definition delete returned nil; expected error")
	}
	if !strings.Contains(err.Error(), "test-simple") && !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error should reference the missing workflow; got %q", err)
	}

	// Task workflow reference must remain intact so the caller can inspect /
	// recover from the error rather than silently losing state.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil {
		t.Error("task.Workflow was cleared on definition-delete error; callers need it for recovery")
	}
	if ti.Workflow.WorkflowID != "test-simple" {
		t.Errorf("task.Workflow.WorkflowID = %q, want %q", ti.Workflow.WorkflowID, "test-simple")
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
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	const callers = 5
	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = engine.StartWorkflow("t1", "test-simple")
		}(i)
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
