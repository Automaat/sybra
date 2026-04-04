package agent

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/tmux"
)

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestManager(t *testing.T) (mgr *Manager, events *[]string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	emitted := &[]string{}
	emit := func(event string, _ any) {
		*emitted = append(*emitted, event)
	}

	m := NewManager(ctx, tmux.NewManager(), emit, discardLogger(), t.TempDir())
	return m, emitted
}

func TestNewManager(t *testing.T) {
	m, _ := newTestManager(t)
	if m == nil {
		t.Fatal("manager is nil")
	}
	if len(m.ListAgents()) != 0 {
		t.Error("expected empty agent list")
	}
}

func TestStartAgentUnknownMode(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.StartAgent("task-1", "Test Task", "invalid", "prompt", nil)
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestStartAgentHeadless(t *testing.T) {
	m, emitted := newTestManager(t)

	// Start headless agent — will fail to run claude but agent entry is created
	a, err := m.StartAgent("task-1", "Test Task", "headless", "test prompt", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if a.ID == "" {
		t.Error("agent ID is empty")
	}
	if a.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want %q", a.TaskID, "task-1")
	}
	if a.Name != "Test Task" {
		t.Errorf("Name = %q, want %q", a.Name, "Test Task")
	}
	if a.Mode != "headless" {
		t.Errorf("Mode = %q, want %q", a.Mode, "headless")
	}
	// State may be Running or Stopped depending on whether the claude binary
	// exists — the headless goroutine exits immediately when it doesn't.
	if a.State != StateRunning && a.State != StateStopped {
		t.Errorf("State = %q, want %q or %q", a.State, StateRunning, StateStopped)
	}

	agents := m.ListAgents()
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}

	if len(*emitted) == 0 {
		t.Error("expected at least one emitted event")
	}
}

func TestGetAgent(t *testing.T) {
	m, _ := newTestManager(t)

	a, err := m.StartAgent("task-1", "Test Task", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := m.GetAgent(a.ID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.ID != a.ID {
		t.Errorf("ID = %q, want %q", got.ID, a.ID)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.GetAgent("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestStopAgent(t *testing.T) {
	m, emitted := newTestManager(t)

	a, err := m.StartAgent("task-1", "Test Task", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	got, err := m.GetAgent(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateStopped {
		t.Errorf("State = %q, want %q", got.State, StateStopped)
	}

	// Should have emitted state events for start and stop
	hasStop := false
	for _, e := range *emitted {
		if e == "agent:state:"+a.ID {
			hasStop = true
		}
	}
	if !hasStop {
		t.Error("expected agent:state event")
	}
}

// TestStopHeadlessDoesNotCallOnComplete verifies that StopAgent for a headless
// agent does not call onComplete immediately — only the goroutine may call it
// after the process exits, preventing premature worktree cleanup.
func TestStopHeadlessDoesNotCallOnComplete(t *testing.T) {
	m, _ := newTestManager(t)

	completeCalls := 0
	m.SetOnComplete(func(_ *Agent) { completeCalls++ })

	a, err := m.StartAgent("task-1", "Test Task", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	// StopAgent must not call onComplete for headless — the goroutine does.
	if completeCalls > 0 {
		t.Errorf("onComplete called %d time(s) by StopAgent, want 0", completeCalls)
	}
}

// TestHasRunningAgentUsesGoroutineLifetime verifies HasRunningAgentForTask
// returns true while the goroutine is alive regardless of State, and false
// only after the goroutine closes done.
func TestHasRunningAgentUsesGoroutineLifetime(t *testing.T) {
	m, _ := newTestManager(t)

	// Manually wire a headless agent with a done channel we control.
	done := make(chan struct{})
	a := &Agent{
		ID:     "test-race",
		TaskID: "task-1",
		Mode:   "headless",
		State:  StateRunning,
		cancel: func() {},
		done:   done,
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()

	if !m.HasRunningAgentForTask("task-1") {
		t.Fatal("expected HasRunningAgentForTask=true before goroutine exits")
	}

	// Simulate StopAgent setting state without goroutine exiting yet.
	a.State = StateStopped

	if !m.HasRunningAgentForTask("task-1") {
		t.Fatal("expected HasRunningAgentForTask=true: goroutine still alive even though state=Stopped")
	}

	// Simulate goroutine exit.
	close(done)

	if m.HasRunningAgentForTask("task-1") {
		t.Fatal("expected HasRunningAgentForTask=false after goroutine exits")
	}
}

func TestStopAgentNotFound(t *testing.T) {
	m, _ := newTestManager(t)
	err := m.StopAgent("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestListAgentsMultiple(t *testing.T) {
	m, _ := newTestManager(t)

	for i := range 3 {
		_, err := m.StartAgent("task-"+string(rune('1'+i)), "Test Task", "headless", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	agents := m.ListAgents()
	if len(agents) != 3 {
		t.Errorf("got %d agents, want 3", len(agents))
	}
}

func TestAgentOutput(t *testing.T) {
	m, _ := newTestManager(t)

	a, err := m.StartAgent("task-1", "Test Task", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Initially empty
	if len(a.Output()) != 0 {
		t.Error("expected empty output buffer")
	}
}

func TestSanitizeSessionName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Implement auth middleware", "implement-auth-middleware"},
		{"  Hello World  ", "hello-world"},
		{"UPPERCASE", "uppercase"},
		{"special!@#chars$%^", "specialchars"},
		{"a-b-c", "a-b-c"},
		{"", "task"},
		{"!!!!", "task"},
		{"a-very-long-title-that-exceeds-the-thirty-character-limit", "a-very-long-title-that-exceeds"},
		{"trailing---dashes---at-cutoff-", "trailing---dashes---at-cutoff"},
	}

	for _, tt := range tests {
		got := sanitizeSessionName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeSessionName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestStartAgentInteractiveSessionName(t *testing.T) {
	requireTmux(t)

	m, _ := newTestManager(t)
	a, err := m.StartAgent("task-1", "Auth Middleware", "interactive", "build it", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAgent(a.ID) })

	if !strings.Contains(a.TmuxSession, "auth-middleware") {
		t.Errorf("TmuxSession = %q, want it to contain %q", a.TmuxSession, "auth-middleware")
	}
	if a.Name != "Auth Middleware" {
		t.Errorf("Name = %q, want %q", a.Name, "Auth Middleware")
	}
}

func TestCapturePaneNotFound(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.CapturePane("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestCapturePaneNoTmuxSession(t *testing.T) {
	m, _ := newTestManager(t)

	a, err := m.StartAgent("task-1", "Test Task", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = m.CapturePane(a.ID)
	if err == nil {
		t.Fatal("expected error for agent without tmux session")
	}
}

func TestCapturePaneStoppedAgent(t *testing.T) {
	m, _ := newTestManager(t)

	a, err := m.StartAgent("task-1", "Test Task", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an interactive agent that was stopped
	a.TmuxSession = "synapse-fake"
	a.State = StateStopped

	out, err := m.CapturePane(a.ID)
	if err != nil {
		t.Fatalf("expected no error for stopped agent, got: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output for stopped agent, got: %q", out)
	}
}

func TestSendInteractivePromptCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	emitted := &[]string{}
	emit := func(event string, _ any) {
		*emitted = append(*emitted, event)
	}

	m := NewManager(ctx, tmux.NewManager(), emit, discardLogger(), t.TempDir())

	a := &Agent{
		ID:          "test-cancel",
		TmuxSession: "synapse-nonexistent",
	}

	// Cancel immediately so sendInteractivePrompt exits via ctx.Done()
	cancel()
	m.sendInteractivePrompt(ctx, a, "test prompt")
	// Should return without error or hang
}

func TestSendInteractivePromptDetectsReady(t *testing.T) {
	requireTmux(t)

	tm := tmux.NewManager()
	session := "synapse-test-ready"
	_ = tm.KillSession(session)

	// Start a session that prints ❯ prompt after brief delay
	err := tm.CreateSession(session, "sh -c 'sleep 0.5 && printf ❯ && sleep 60'")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	m := NewManager(ctx, tm, func(string, any) {}, discardLogger(), t.TempDir())
	a := &Agent{ID: "test-ready", TmuxSession: session}

	done := make(chan struct{})
	go func() {
		m.sendInteractivePrompt(ctx, a, "hello world")
		close(done)
	}()

	select {
	case <-done:
		// Prompt was sent successfully
	case <-time.After(10 * time.Second):
		t.Fatal("sendInteractivePrompt did not complete in time")
	}
}

// newInteractiveAgent creates a fake interactive agent backed by a real tmux
// session running a simple command (no claude dependency).
func newInteractiveAgent(t *testing.T, m *Manager) *Agent {
	t.Helper()
	tm := tmux.NewManager()
	session := "synapse-test-" + t.Name()
	_ = tm.KillSession(session)

	if err := tm.CreateSession(session, "sleep 5"); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })

	a := &Agent{
		ID:          "test-" + t.Name(),
		TaskID:      "task-1",
		Mode:        "interactive",
		State:       StateRunning,
		TmuxSession: session,
		cancel:      func() {},
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	return a
}

func TestStopInteractiveAgent(t *testing.T) {
	requireTmux(t)

	m, _ := newTestManager(t)
	a := newInteractiveAgent(t, m)

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	if a.State != StateStopped {
		t.Errorf("State = %q, want %q", a.State, StateStopped)
	}

	tm := tmux.NewManager()
	if tm.SessionExists(a.TmuxSession) {
		t.Error("tmux session should not exist after stop")
	}
}

func TestCapturePaneInteractiveRunning(t *testing.T) {
	requireTmux(t)

	m, _ := newTestManager(t)
	a := newInteractiveAgent(t, m)

	_, err := m.CapturePane(a.ID)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
}

func TestCapturePaneAfterStop(t *testing.T) {
	requireTmux(t)

	m, _ := newTestManager(t)
	a := newInteractiveAgent(t, m)

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	out, err := m.CapturePane(a.ID)
	if err != nil {
		t.Fatalf("expected no error after stop, got: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output after stop, got: %q", out)
	}
}

func TestHasRunningAgentForTask(t *testing.T) {
	m, _ := newTestManager(t)

	// No agents — always false.
	if m.HasRunningAgentForTask("task-1") {
		t.Error("expected false with no agents")
	}

	// Manually register a running agent for task-1.
	running := &Agent{ID: "a1", TaskID: "task-1", State: StateRunning, cancel: func() {}}
	m.mu.Lock()
	m.agents["a1"] = running
	m.mu.Unlock()

	if !m.HasRunningAgentForTask("task-1") {
		t.Error("expected true for running agent on task-1")
	}
	if m.HasRunningAgentForTask("task-2") {
		t.Error("expected false for different task")
	}

	// Stopped agent — should return false.
	running.State = StateStopped
	if m.HasRunningAgentForTask("task-1") {
		t.Error("expected false for stopped agent")
	}
}

func TestShutdown(t *testing.T) {
	m, _ := newTestManager(t)

	for range 3 {
		_, err := m.StartAgent("task-1", "Test Task", "headless", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	m.Shutdown()

	// All agents should still be in the map (shutdown doesn't remove them)
	if len(m.ListAgents()) != 3 {
		t.Errorf("got %d agents, want 3", len(m.ListAgents()))
	}
}
