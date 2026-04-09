package agent

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/events"
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

// eventRecorder provides concurrency-safe access to the stream of event
// names produced by the manager under test. Runner goroutines append via
// the Manager's emit callback while the test goroutine reads via Len /
// Snapshot — both paths must be synchronized under the race detector.
type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *eventRecorder) add(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

// Len returns the current number of recorded events.
func (r *eventRecorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// Snapshot returns a copy of the recorded events for safe iteration.
func (r *eventRecorder) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

func newTestManager(t *testing.T) (mgr *Manager, emitted *eventRecorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	emitted = &eventRecorder{}
	emit := func(event string, _ any) {
		emitted.add(event)
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
	st := a.GetState()
	if st != StateRunning && st != StateStopped {
		t.Errorf("State = %q, want %q or %q", st, StateRunning, StateStopped)
	}

	agents := m.ListAgents()
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}

	if emitted.Len() == 0 {
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
	if got.GetState() != StateStopped {
		t.Errorf("State = %q, want %q", got.GetState(), StateStopped)
	}

	// Should have emitted state events for start and stop
	hasStop := false
	for _, e := range emitted.Snapshot() {
		if e == events.AgentState(a.ID) {
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

	// Counter is written from the runner goroutine (onComplete path) and
	// read from the test goroutine after StopAgent returns — protect
	// with a mutex for the race detector.
	var (
		mu            sync.Mutex
		completeCalls int
	)
	m.SetOnComplete(func(_ *Agent) {
		mu.Lock()
		completeCalls++
		mu.Unlock()
	})

	a, err := m.StartAgent("task-1", "Test Task", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	// StopAgent must not call onComplete for headless — the goroutine does.
	mu.Lock()
	got := completeCalls
	mu.Unlock()
	if got > 0 {
		t.Errorf("onComplete called %d time(s) by StopAgent, want 0", got)
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
	a.SetState(StateStopped)

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

func TestStartAgentInteractiveConversational(t *testing.T) {
	m, _ := newTestManager(t)
	a, err := m.StartAgent("task-1", "Auth Middleware", "interactive", "build it", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAgent(a.ID) })

	if a.Name != "Auth Middleware" {
		t.Errorf("Name = %q, want %q", a.Name, "Auth Middleware")
	}
	if a.Mode != "interactive" {
		t.Errorf("Mode = %q, want %q", a.Mode, "interactive")
	}
	if a.done == nil {
		t.Error("interactive agent should have done channel (conversational)")
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
	a.SetState(StateStopped)

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

	emit := func(string, any) {}

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
	m, _ := newTestManager(t)

	// Create a conversational-style interactive agent (no tmux).
	a := &Agent{
		ID:     "test-stop",
		TaskID: "task-1",
		Mode:   "interactive",
		State:  StateRunning,
		done:   make(chan struct{}),
		cancel: func() {},
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	if st := a.GetState(); st != StateStopped {
		t.Errorf("State = %q, want %q", st, StateStopped)
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
	running.SetState(StateStopped)
	if m.HasRunningAgentForTask("task-1") {
		t.Error("expected false for stopped agent")
	}
}

func TestBuildCommand(t *testing.T) {
	m, _ := newTestManager(t)

	tests := []struct {
		name    string
		cfg     RunConfig
		wantCmd string
		wantErr bool
	}{
		{
			name:    "no model no tools",
			cfg:     RunConfig{},
			wantCmd: "claude --dangerously-skip-permissions --model sonnet",
		},
		{
			name:    "valid model",
			cfg:     RunConfig{Model: "claude-opus-4-6"},
			wantCmd: "claude --dangerously-skip-permissions --model claude-opus-4-6",
		},
		{
			name:    "valid tools",
			cfg:     RunConfig{AllowedTools: []string{"Read", "Write", "Bash"}},
			wantCmd: "claude --allowedTools Read,Write,Bash --model sonnet",
		},
		{
			name:    "valid model and tools",
			cfg:     RunConfig{Model: "claude-sonnet-4-6", AllowedTools: []string{"Read"}},
			wantCmd: "claude --allowedTools Read --model claude-sonnet-4-6",
		},
		{
			name:    "model with shell metachar semicolon",
			cfg:     RunConfig{Model: "claude;rm -rf /"},
			wantErr: true,
		},
		{
			name:    "model with shell metachar backtick",
			cfg:     RunConfig{Model: "claude`whoami`"},
			wantErr: true,
		},
		{
			name:    "model with shell metachar dollar",
			cfg:     RunConfig{Model: "claude$(id)"},
			wantErr: true,
		},
		{
			name:    "model with shell metachar pipe",
			cfg:     RunConfig{Model: "claude|cat /etc/passwd"},
			wantErr: true,
		},
		{
			name:    "model with shell metachar space",
			cfg:     RunConfig{Model: "claude sonnet"},
			wantErr: true,
		},
		{
			name:    "tool with shell metachar semicolon",
			cfg:     RunConfig{AllowedTools: []string{"Read", "Bash;rm -rf /"}},
			wantErr: true,
		},
		{
			name:    "tool with shell metachar ampersand",
			cfg:     RunConfig{AllowedTools: []string{"Read&&whoami"}},
			wantErr: true,
		},
		{
			name:    "tool with shell metachar newline",
			cfg:     RunConfig{AllowedTools: []string{"Read\nrm -rf /"}},
			wantErr: true,
		},
		{
			name:    "valid model with slash and dot",
			cfg:     RunConfig{Model: "anthropic/claude-3.5-sonnet"},
			wantCmd: "claude --dangerously-skip-permissions --model anthropic/claude-3.5-sonnet",
		},
		{
			name:    "codex default model mapping",
			cfg:     RunConfig{Provider: "codex"},
			wantCmd: "codex exec --json --skip-git-repo-check --full-auto --model gpt-5.4",
		},
		{
			name:    "codex maps haiku to mini",
			cfg:     RunConfig{Provider: "codex", Model: "haiku"},
			wantCmd: "codex exec --json --skip-git-repo-check --full-auto --model gpt-5.4-mini",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.buildCommand(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got cmd=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantCmd {
				t.Errorf("cmd = %q, want %q", got, tt.wantCmd)
			}
		})
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

// --- Plan-review fix: paused conversational agents must stay findable ---
//
// Interactive (conversational) agents flip to StatePaused after each
// turn's result event. The plan-review UI relies on FindRunningAgentForTask
// to locate the paused agent so a follow-up prompt can be delivered
// without spawning a new session. These tests lock in that behaviour.

// putAgent registers a fully-formed Agent in the manager without running
// any goroutine — used so tests can arrange arbitrary state fields
// (State, stdinPipe, TmuxSession) deterministically.
func putAgent(t *testing.T, m *Manager, a *Agent) {
	t.Helper()
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
}

func TestFindRunningAgentForTask_ByState(t *testing.T) {
	tests := []struct {
		name     string
		state    State
		wantLive bool
	}{
		{name: "running agent is live", state: StateRunning, wantLive: true},
		{name: "paused agent is live", state: StatePaused, wantLive: true},
		{name: "stopped agent is not live", state: StateStopped, wantLive: false},
		{name: "idle agent is not live", state: StateIdle, wantLive: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			putAgent(t, m, &Agent{
				ID:     "a1",
				TaskID: "task-1",
				Name:   "plan:Test Task",
				Mode:   "interactive",
				State:  tt.state,
			})

			got := m.FindRunningAgentForTask("task-1", RolePlan)

			if tt.wantLive && got == nil {
				t.Fatalf("FindRunningAgentForTask returned nil for %s agent, want the agent", tt.state)
			}
			if !tt.wantLive && got != nil {
				t.Fatalf("FindRunningAgentForTask returned agent for %s state, want nil", tt.state)
			}
		})
	}
}

func TestFindRunningAgentForTask_FiltersByRoleAndTask(t *testing.T) {
	m, _ := newTestManager(t)

	putAgent(t, m, &Agent{ID: "a1", TaskID: "task-1", Name: "plan:A", State: StatePaused})
	putAgent(t, m, &Agent{ID: "a2", TaskID: "task-1", Name: "triage:A", State: StatePaused})
	putAgent(t, m, &Agent{ID: "a3", TaskID: "task-2", Name: "plan:B", State: StatePaused})

	got := m.FindRunningAgentForTask("task-1", RolePlan)

	if got == nil {
		t.Fatal("expected to find a1")
	}
	if got.ID != "a1" {
		t.Errorf("ID = %q, want a1", got.ID)
	}
}

// captureStdinAgent wires up a real io.Pipe so a test can observe
// exactly what SendPromptToAgent writes to the conversational agent's
// stdin. Returns the agent (already registered in m) and a channel that
// receives one line per message written.
func captureStdinAgent(t *testing.T, m *Manager, id, taskID string) (ag *Agent, stdinLines <-chan string) {
	t.Helper()
	r, w := io.Pipe()
	a := &Agent{
		ID:        id,
		TaskID:    taskID,
		Name:      "plan:Test",
		Mode:      "interactive",
		State:     StatePaused,
		stdinPipe: w,
	}
	putAgent(t, m, a)

	lines := make(chan string, 4)
	go func() {
		defer close(lines)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				lines <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = r.Close() })
	return a, lines
}

func TestSendPromptToAgent_WritesToStdinForConversationalAgent(t *testing.T) {
	m, _ := newTestManager(t)
	a, lines := captureStdinAgent(t, m, "a1", "task-1")

	err := m.SendPromptToAgent(a.ID, "please revise")

	if err != nil {
		t.Fatalf("SendPromptToAgent: %v", err)
	}

	select {
	case got := <-lines:
		// SendMessage encodes a JSON user message; verify both the
		// envelope and the payload without being sensitive to exact
		// field ordering.
		if !strings.Contains(got, `"type":"user"`) {
			t.Errorf("stdin payload missing user envelope: %q", got)
		}
		if !strings.Contains(got, `please revise`) {
			t.Errorf("stdin payload missing message text: %q", got)
		}
		if !strings.HasSuffix(got, "\n") {
			t.Errorf("stdin payload not newline-terminated: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no data written to stdin within 2s")
	}

	// The agent must have been flipped back to Running so downstream
	// code treats the next turn as active work.
	if st := a.GetState(); st != StateRunning {
		t.Errorf("State = %q, want %q after send", st, StateRunning)
	}
}

func TestSendPromptToAgent_RejectsStoppedAgent(t *testing.T) {
	m, _ := newTestManager(t)
	putAgent(t, m, &Agent{
		ID:     "dead",
		TaskID: "task-1",
		Mode:   "interactive",
		State:  StateStopped,
	})

	err := m.SendPromptToAgent("dead", "anything")

	if err == nil {
		t.Fatal("expected error sending to stopped agent")
	}
}

func TestSendPromptToAgent_RejectsAgentWithoutTransport(t *testing.T) {
	m, _ := newTestManager(t)
	// Interactive but no stdin pipe and no tmux session — neither
	// transport is available.
	putAgent(t, m, &Agent{
		ID:     "orphan",
		TaskID: "task-1",
		Mode:   "interactive",
		State:  StateRunning,
	})

	err := m.SendPromptToAgent("orphan", "hello")

	if err == nil {
		t.Fatal("expected error when agent has no transport")
	}
}
