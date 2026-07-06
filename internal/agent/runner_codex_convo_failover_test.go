package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFakePerTurnBinary drops an executable shell script named `name` on
// PATH that ignores its arguments and prints the given NDJSON lines, one per
// line, to stdout. Used to simulate codex/copilot per-turn CLIs without a
// real provider.
func writeFakePerTurnBinary(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	script := "#!/bin/sh\n"
	for _, l := range lines {
		script += "printf '%s\\n' " + shQuote(l) + "\n"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

// shQuote wraps s in single quotes for embedding in a generated shell script,
// escaping any literal single quotes it contains.
func shQuote(s string) string {
	escaped := ""
	for _, r := range s {
		if r == '\'' {
			escaped += `'\''`
		} else {
			escaped += string(r)
		}
	}
	return "'" + escaped + "'"
}

func waitForAgentState(t *testing.T, a *Agent, want State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if a.GetState() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("agent did not reach state %s within %s (last state %s)", want, timeout, a.GetState())
}

func waitForAgentProvider(t *testing.T, a *Agent, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if a.GetProvider() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("agent provider never became %q within %s (last %q)", want, timeout, a.GetProvider())
}

// TestRunPerTurnConversational_FailsOverMidConversation is an end-to-end
// regression test for the core feature: a per-turn conversational agent
// (codex) whose provider caps mid-conversation must hot-swap to a healthy
// per-turn-capable peer (copilot) on its NEXT turn, without the caller
// spawning a doomed codex process and without losing the conversation.
func TestRunPerTurnConversational_FailsOverMidConversation(t *testing.T) {
	dir := t.TempDir()
	writeFakePerTurnBinary(t, dir, "codex",
		`{"type":"thread.started","thread_id":"cx-fake"}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	)
	writeFakePerTurnBinary(t, dir, "copilot",
		`{"type":"assistant.message","data":{"content":"hi from copilot"}}`,
		`{"type":"result","sessionId":"cop-fake"}`,
	)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"codex": true, "copilot": true}})

	sessionDir := t.TempDir()
	a := &Agent{ID: "conv1", TaskID: "task1", Provider: "codex", Mode: "interactive", sessionCWD: sessionDir, done: make(chan struct{})}
	a.setPromptChannel(make(chan string, 1))
	m.mu.Lock()
	m.agents[a.ID] = a
	m.liveByProvider["codex"] = 1
	m.liveCount = 1
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.cancel = cancel
	go m.runPerTurnConversational(ctx, a, RunConfig{Prompt: "hi", Provider: "codex", RequirePermissions: true}, false)

	waitForAgentState(t, a, StatePaused, 3*time.Second)
	if a.GetProvider() != "codex" {
		t.Fatalf("expected first turn to run on codex, got %s", a.GetProvider())
	}

	// codex caps mid-conversation; copilot is the only healthy per-turn peer.
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"codex": false, "copilot": true},
		reasons: map[string]string{"codex": "rate_limited"},
	})

	if err := m.sendConvoPrompt(a.ID, "continue"); err != nil {
		t.Fatalf("sendConvoPrompt: %v", err)
	}

	waitForAgentProvider(t, a, "copilot", 3*time.Second)
	waitForAgentState(t, a, StatePaused, 3*time.Second)

	if a.GetSessionID() != "cop-fake" {
		t.Errorf("expected copilot's session id captured after switch, got %q", a.GetSessionID())
	}

	m.mu.RLock()
	codexCount, codexOK := m.liveByProvider["codex"]
	copilotCount := m.liveByProvider["copilot"]
	m.mu.RUnlock()
	if codexOK && codexCount != 0 {
		t.Errorf("codex live count should be gone after switch, got %d", codexCount)
	}
	if copilotCount != 1 {
		t.Errorf("copilot live count should be 1 after switch, got %d", copilotCount)
	}

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	select {
	case <-a.done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not exit after StopAgent")
	}
}

// TestRunPerTurnConversational_NoPeerParksInstead verifies that when a
// per-turn agent's provider caps and no healthy per-turn peer exists, the
// runner does not spawn a doomed turn: it exits with a rate_limit error kind
// (compatible with the existing reschedule/park behavior) instead of
// reporting false success or silently hanging.
func TestRunPerTurnConversational_NoPeerParksInstead(t *testing.T) {
	dir := t.TempDir()
	writeFakePerTurnBinary(t, dir, "codex",
		`{"type":"thread.started","thread_id":"cx-fake"}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"codex": true}})

	sessionDir := t.TempDir()
	a := &Agent{ID: "conv2", TaskID: "task2", Provider: "codex", Mode: "interactive", sessionCWD: sessionDir, done: make(chan struct{})}
	a.setPromptChannel(make(chan string, 1))
	m.mu.Lock()
	m.agents[a.ID] = a
	m.liveByProvider["codex"] = 1
	m.liveCount = 1
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go m.runPerTurnConversational(ctx, a, RunConfig{Prompt: "hi", Provider: "codex"}, false)

	waitForAgentState(t, a, StatePaused, 3*time.Second)

	// codex caps with no healthy per-turn peer at all (copilot unhealthy too,
	// claude is never a valid per-turn hot-swap target).
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"codex": false, "copilot": false, "claude": true},
		reasons: map[string]string{"codex": "rate_limited"},
	})

	if err := m.sendConvoPrompt(a.ID, "continue"); err != nil {
		t.Fatalf("sendConvoPrompt: %v", err)
	}

	select {
	case <-a.done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not finalize after a blocked re-gate")
	}

	if a.GetErrorKind() != "rate_limit" {
		t.Errorf("expected rate_limit error kind on no-peer block, got %q", a.GetErrorKind())
	}
	if a.GetProvider() != "codex" {
		t.Errorf("provider must be unchanged when no switch happens, got %q", a.GetProvider())
	}
}
