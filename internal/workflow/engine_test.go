package workflow

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("testdata", "test-simple.yaml"))
	if err != nil {
		t.Fatalf("read test workflow: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test-simple.yaml"), src, 0o644); err != nil {
		t.Fatal(err)
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
	TaskID, Role, Mode, Model, Prompt string
	AllowedTools                      []string
	NeedsWorktree                     bool
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

func (m *mockAgents) StartAgent(taskID, role, mode, model, prompt string, allowedTools []string, needsWorktree bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	id := fmt.Sprintf("agent-%d", m.counter)
	m.calls = append(m.calls, startCall{
		TaskID: taskID, Role: role, Mode: mode, Model: model,
		Prompt: prompt, AllowedTools: allowedTools, NeedsWorktree: needsWorktree,
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

func TestNoWorkflowField(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"}) // no Workflow

	// Should not panic or error fatally.
	engine.HandleAgentComplete("t1", "agent-1", "result")
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
	agents.StartAgent("t1", "implementation", "headless", "sonnet", "test", nil, false)

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

	// Another agent complete on an already-completed workflow should not panic.
	callsBefore := agents.CallCount()
	engine.HandleAgentComplete("t1", "stale-agent", "late result")

	if agents.CallCount() != callsBefore {
		t.Error("HandleAgentComplete on completed workflow should not start new agents")
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
