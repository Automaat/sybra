package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/events"
)

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

	m := NewManager(ctx, emit, discardLogger(), t.TempDir())
	return m, emitted
}

// startTestAgent starts an agent in a fresh working directory, satisfying the
// Run guard that requires a valid, existing working dir. Uses os.MkdirTemp
// with a best-effort cleanup (not t.TempDir) because background goroutines
// spawned by runHeadless may still be touching the dir when the test ends.
func startTestAgent(t *testing.T, m *Manager, taskID, title, mode, prompt string, allowedTools []string) (*Agent, error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sybra-agent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return m.StartAgent(taskID, title, mode, prompt, dir, allowedTools)
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
	_, err := startTestAgent(t, m, "task-1", "Test Task", "invalid", "prompt", nil)
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestStartAgentHeadless(t *testing.T) {
	m, emitted := newTestManager(t)

	// Start headless agent — will fail to run claude but agent entry is created
	a, err := startTestAgent(t, m, "task-1", "Test Task", "headless", "test prompt", nil)
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

func TestRunConfigResumeSessionID(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	dir, err := os.MkdirTemp("", "sybra-agent-resume-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ag, err := m.Run(RunConfig{
		TaskID:          "task-resume",
		Name:            "Resume Test",
		Mode:            "headless",
		Prompt:          "continue",
		Dir:             dir,
		ResumeSessionID: "ses-resume-abc",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ag.GetSessionID() != "ses-resume-abc" {
		t.Errorf("SessionID = %q, want %q", ag.GetSessionID(), "ses-resume-abc")
	}
}

func TestGetAgent(t *testing.T) {
	m, _ := newTestManager(t)

	a, err := startTestAgent(t, m, "task-1", "Test Task", "headless", "test", nil)
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

	a, err := startTestAgent(t, m, "task-1", "Test Task", "headless", "test", nil)
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

	a, err := startTestAgent(t, m, "task-1", "Test Task", "headless", "test", nil)
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

func TestRunningCountTracksLiveAgents(t *testing.T) {
	m, _ := newTestManager(t)

	a1 := &Agent{ID: "a1", TaskID: "task-1", State: StateRunning, done: make(chan struct{})}
	a2 := &Agent{ID: "a2", TaskID: "task-2", State: StatePaused, done: make(chan struct{})}
	m.mu.Lock()
	m.agents[a1.ID] = a1
	m.agents[a2.ID] = a2
	m.liveCount = 2
	m.mu.Unlock()

	if got := m.RunningCount(); got != 2 {
		t.Fatalf("RunningCount = %d, want 2", got)
	}

	m.markAgentDone(a1)
	if got := m.RunningCount(); got != 1 {
		t.Fatalf("RunningCount after one done = %d, want 1", got)
	}

	// Idempotent on repeated terminal paths.
	m.markAgentDone(a1)
	if got := m.RunningCount(); got != 1 {
		t.Fatalf("RunningCount after duplicate done = %d, want 1", got)
	}

	m.markAgentDone(a2)
	if got := m.RunningCount(); got != 0 {
		t.Fatalf("RunningCount after all done = %d, want 0", got)
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
		_, err := startTestAgent(t, m, "task-"+string(rune('1'+i)), "Test Task", "headless", "test", nil)
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

	a, err := startTestAgent(t, m, "task-1", "Test Task", "headless", "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Initially empty
	if len(a.Output()) != 0 {
		t.Error("expected empty output buffer")
	}
}

func TestStartAgentInteractiveConversational(t *testing.T) {
	m, _ := newTestManager(t)
	a, err := startTestAgent(t, m, "task-1", "Auth Middleware", "interactive", "build it", nil)
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

func TestStopInteractiveAgent(t *testing.T) {
	m, _ := newTestManager(t)

	// Create a conversational-style interactive agent.
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
		{
			name:    "codex with RequirePermissions uses workspace-write sandbox",
			cfg:     RunConfig{Provider: "codex", RequirePermissions: true},
			wantCmd: "codex exec --json --skip-git-repo-check --sandbox workspace-write --model gpt-5.4",
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

func TestCodexSandboxDisabledViaEnv(t *testing.T) {
	t.Setenv("SYBRA_DISABLE_CODEX_SANDBOX", "1")
	m, _ := newTestManager(t)

	tests := []struct {
		name    string
		cfg     RunConfig
		wantCmd string
	}{
		{
			name:    "codex default with sandbox disabled",
			cfg:     RunConfig{Provider: "codex"},
			wantCmd: "codex exec --json --skip-git-repo-check --sandbox danger-full-access --model gpt-5.4",
		},
		{
			name:    "codex RequirePermissions honored as danger-full-access when sandbox disabled",
			cfg:     RunConfig{Provider: "codex", RequirePermissions: true},
			wantCmd: "codex exec --json --skip-git-repo-check --sandbox danger-full-access --model gpt-5.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.buildCommand(tt.cfg)
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
		_, err := startTestAgent(t, m, "task-1", "Test Task", "headless", "test", nil)
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

// TestShutdownWithGrace_WaitsForDoneChannels locks in the graceful-shutdown
// contract introduced after the 2026-04-16 "signal: killed" wave: once the
// agent manager cancels running agents, it must block on their done
// channels until either they close or the grace window elapses. Before
// this fix, Shutdown returned immediately and the server process exited
// mid-stream, truncating NDJSON result lines.
func TestShutdownWithGrace_WaitsForDoneChannels(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	// Seed a fake running agent: registered in m.agents with a done
	// channel the test closes from a goroutine to simulate clean exit.
	done := make(chan struct{})
	_, agCancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.agents["ag1"] = &Agent{
		ID:     "ag1",
		cancel: agCancel,
		done:   done,
	}
	m.mu.Unlock()

	// Close done after a short delay — models a well-behaved subprocess
	// noticing SIGTERM and flushing its result before exiting.
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(done)
	}()

	start := time.Now()
	m.ShutdownWithGrace(500 * time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 300*time.Millisecond {
		t.Errorf("shutdown took %s, expected it to return shortly after done close", elapsed)
	}
	if elapsed < 30*time.Millisecond {
		t.Errorf("shutdown took %s, must have waited for done channel", elapsed)
	}
}

// TestShutdownWithGrace_RespectsGraceDeadline guards against the opposite
// failure: a hung agent must not block shutdown forever. After the grace
// window the helper returns and the outer server shutdown can complete.
func TestShutdownWithGrace_RespectsGraceDeadline(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	done := make(chan struct{})
	_, agCancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.agents["stuck"] = &Agent{
		ID:     "stuck",
		cancel: agCancel,
		done:   done, // never closed
	}
	m.mu.Unlock()

	start := time.Now()
	m.ShutdownWithGrace(80 * time.Millisecond)
	elapsed := time.Since(start)
	if elapsed < 80*time.Millisecond {
		t.Errorf("returned early at %s, expected to wait the full grace", elapsed)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("waited %s, expected to bail around the grace window", elapsed)
	}
}

func TestShutdownWithGrace_NoAgents(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)
	start := time.Now()
	m.ShutdownWithGrace(5 * time.Second)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("empty shutdown took %s, should return immediately", elapsed)
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
// (State, stdinPipe) deterministically.
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
	// Interactive but no stdin pipe or prompt channel — no transport available.
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

// TestSendMessage_QueuesWhenRunning verifies the queue-on-busy behaviour:
// a follow-up sent mid-turn (StateRunning) must not hit stdin and must not
// flip state — it just lives in pendingPrompts until the next result.
// Regression guard for the chat-input "queue follow-up" feature.
func TestSendMessage_QueuesWhenRunning(t *testing.T) {
	m, _ := newTestManager(t)
	a, lines := captureStdinAgent(t, m, "busy", "task-1")
	a.SetState(StateRunning) // simulate mid-turn

	if err := m.SendMessage(a.ID, "queued text"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Nothing should land on stdin while the agent is mid-turn.
	select {
	case got := <-lines:
		t.Fatalf("expected no stdin write while running, got %q", got)
	case <-time.After(150 * time.Millisecond):
	}

	if got := a.PendingPromptCount(); got != 1 {
		t.Fatalf("PendingPromptCount = %d, want 1", got)
	}
	if st := a.GetState(); st != StateRunning {
		t.Errorf("State = %q, want %q (queued send must not flip state)", st, StateRunning)
	}

	// User message must still appear in the convo buffer immediately so
	// the chat UI shows the queued bubble before the turn drains.
	convo := a.ConvoOutput()
	if len(convo) != 1 {
		t.Fatalf("convo len = %d, want 1", len(convo))
	}
	if convo[0].Type != "user_input" || convo[0].Text != "queued text" {
		t.Errorf("convo[0] = %+v, want user_input/queued text", convo[0])
	}
}

// TestSendMessage_WritesStdinWhenPaused covers the idle-chat path: when the
// agent is parked in StatePaused (e.g. fresh chat with no initial prompt),
// the next SendMessage must hit stdin directly and flip back to Running.
func TestSendMessage_WritesStdinWhenPaused(t *testing.T) {
	m, _ := newTestManager(t)
	a, lines := captureStdinAgent(t, m, "idle", "task-1")
	// captureStdinAgent already starts in StatePaused.

	if err := m.SendMessage(a.ID, "first message"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	select {
	case got := <-lines:
		if !strings.Contains(got, `first message`) {
			t.Errorf("stdin payload missing text: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no stdin write within 2s")
	}

	if got := a.PendingPromptCount(); got != 0 {
		t.Errorf("PendingPromptCount = %d, want 0", got)
	}
	if st := a.GetState(); st != StateRunning {
		t.Errorf("State = %q, want %q after immediate send", st, StateRunning)
	}
}

// TestStreamConvoOutput_FlushesQueueOnResult verifies that streamConvoOutput
// drains the pending prompt queue when a result event arrives — the next
// queued prompt is written to stdin and the agent stays Running. Without
// this drain, queued chat follow-ups would never reach claude.
func TestStreamConvoOutput_FlushesQueueOnResult(t *testing.T) {
	m, _ := newTestManager(t)
	a, lines := captureStdinAgent(t, m, "drain", "task-1")
	a.SetState(StateRunning)
	a.EnqueuePrompt("next turn please")

	resultLine := `{"type":"result","subtype":"success","session_id":"s-1","total_cost_usd":0.1,"usage":{"input_tokens":10,"output_tokens":5}}` + "\n"
	m.streamConvoOutput(a, strings.NewReader(resultLine), nil, false)

	select {
	case got := <-lines:
		if !strings.Contains(got, `next turn please`) {
			t.Errorf("queued prompt not drained to stdin: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued prompt was not written to stdin within 2s")
	}

	if got := a.PendingPromptCount(); got != 0 {
		t.Errorf("PendingPromptCount after drain = %d, want 0", got)
	}
	if st := a.GetState(); st != StateRunning {
		t.Errorf("State = %q, want Running while queue still has work in flight", st)
	}
}

// TestStreamConvoOutput_PausesWhenQueueEmpty verifies the default no-queue
// path: a result event with an empty pending queue must flip the agent to
// StatePaused so the chat input goes from "thinking" to typeable.
func TestStreamConvoOutput_PausesWhenQueueEmpty(t *testing.T) {
	m, _ := newTestManager(t)
	a, _ := captureStdinAgent(t, m, "calm", "task-1")
	a.SetState(StateRunning)

	resultLine := `{"type":"result","subtype":"success","session_id":"s-1","total_cost_usd":0.05}` + "\n"
	m.streamConvoOutput(a, strings.NewReader(resultLine), nil, false)

	if st := a.GetState(); st != StatePaused {
		t.Errorf("State = %q, want %q after result with empty queue", st, StatePaused)
	}
}
