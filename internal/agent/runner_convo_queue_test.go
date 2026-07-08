package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeClaudeConvoScriptSlow mirrors fakeClaudeConvoScript but sleeps briefly
// after reading each line before replying, giving a test a window to enqueue
// a follow-up message (via SendMessage's queued branch) while the turn is
// still StateRunning.
const fakeClaudeConvoScriptSlow = "#!/bin/sh\n" +
	"while IFS= read -r line; do\n" +
	"  sleep 0.3\n" +
	"  printf '%s\\n' '{\"type\":\"system\",\"session_id\":\"claude-sess\"}'\n" +
	"  printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"claude-sess\"}'\n" +
	"done\n" +
	"exit 0\n"

// startSlowPersistentClaude spins up a persistent-Claude conversational agent
// backed by fakeClaudeConvoScriptSlow and waits for the initial turn to
// settle at StatePaused, ready for a follow-up.
func startSlowPersistentClaude(t *testing.T, id string) (*Manager, *Agent) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(fakeClaudeConvoScriptSlow), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	writeFakePerTurnBinary(t, dir, "copilot",
		`{"type":"assistant.message","data":{"content":"hi from copilot"}}`,
		`{"type":"result","sessionId":"cop-fake"}`,
	)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "copilot": true}})

	sessionDir := t.TempDir()
	a := &Agent{ID: id, TaskID: "t-" + id, Provider: "claude", Mode: "interactive", sessionCWD: sessionDir, done: make(chan struct{})}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.liveByProvider["claude"] = 1
	m.liveCount = 1
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	a.cancel = cancel
	go m.runConversational(ctx, a, RunConfig{Prompt: "hi", Provider: "claude"})

	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)
	return m, a
}

// TestQueuedFlush_RegatesAndHotSwapsAtTurnBoundary is the permanent
// regression test for the bug reported by adversarial testing: a follow-up
// queued while a persistent-Claude turn is running must be re-gated (not
// written directly to the now-capped Claude stdin) when it is flushed after
// the turn's result.
func TestQueuedFlush_RegatesAndHotSwapsAtTurnBoundary(t *testing.T) {
	m, a := startSlowPersistentClaude(t, "q1")

	if err := m.SendMessage(a.ID, "turn2"); err != nil {
		t.Fatalf("SendMessage turn2: %v", err)
	}
	waitForAgentState(t, a, StateRunning, testStateWaitTimeout)

	// Enqueue a follow-up while turn2 is still running (sleeping in the fake
	// script), then flip Claude unhealthy before its result arrives.
	if err := m.SendMessage(a.ID, "turn3"); err != nil {
		t.Fatalf("SendMessage turn3 (queued): %v", err)
	}
	if a.PendingPromptCount() != 1 {
		t.Fatalf("expected turn3 queued, PendingPromptCount=%d", a.PendingPromptCount())
	}
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"claude": false, "copilot": true},
		reasons: map[string]string{"claude": "rate_limited"},
	})

	waitForAgentProvider(t, a, "copilot", testStateWaitTimeout)
	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)

	if a.GetSessionID() != "cop-fake" {
		t.Errorf("expected copilot session id after queued-flush handoff, got %q", a.GetSessionID())
	}
	if a.PendingPromptCount() != 0 {
		t.Errorf("queued prompt should have been consumed by the handoff, remaining=%d", a.PendingPromptCount())
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

// TestQueuedFlush_NoPeerRestoresPromptToFront verifies that when the queued
// flush finds no healthy peer, the prompt is restored to the front of the
// queue (not lost) and the agent parks with a rate_limit error instead of
// writing to the capped Claude stdin.
func TestQueuedFlush_NoPeerRestoresPromptToFront(t *testing.T) {
	m, a := startSlowPersistentClaude(t, "q2")

	if err := m.SendMessage(a.ID, "turn2"); err != nil {
		t.Fatalf("SendMessage turn2: %v", err)
	}
	waitForAgentState(t, a, StateRunning, testStateWaitTimeout)

	if err := m.SendMessage(a.ID, "turn3"); err != nil {
		t.Fatalf("SendMessage turn3 (queued): %v", err)
	}
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"claude": false, "codex": false, "copilot": false},
		reasons: map[string]string{"claude": "rate_limited"},
	})

	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)

	if a.GetProvider() != "claude" {
		t.Errorf("provider must stay claude when no peer is healthy, got %q", a.GetProvider())
	}
	if a.GetErrorKind() != "rate_limit" {
		t.Errorf("expected rate_limit error kind, got %q", a.GetErrorKind())
	}
	if got, ok := a.PopPendingPrompt(); !ok || got != "turn3" {
		t.Errorf("expected turn3 restored to front of queue, got %q ok=%v", got, ok)
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

// TestQueuedFlush_CarriesRemainingQueueInOrderAcrossHandoff verifies that
// when a queued flush hot-swaps to a per-turn peer, any further prompts still
// sitting in the same Agent's pending queue survive the handoff and replay
// on the peer in original order, without waiting on a new SendPromptToAgent
// call.
func TestQueuedFlush_CarriesRemainingQueueInOrderAcrossHandoff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(fakeClaudeConvoScriptSlow), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	writeFakePerTurnBinary(t, dir, "copilot",
		`{"type":"assistant.message","data":{"content":"hi from copilot"}}`,
		`{"type":"result","sessionId":"cop-fake"}`,
	)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true, "copilot": true}})

	sessionDir := t.TempDir()
	a := &Agent{ID: "q3", TaskID: "t-q3", Provider: "claude", Mode: "interactive", sessionCWD: sessionDir, done: make(chan struct{})}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.liveByProvider["claude"] = 1
	m.liveCount = 1
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	a.cancel = cancel
	go m.runConversational(ctx, a, RunConfig{Prompt: "hi", Provider: "claude"})
	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)

	if err := m.SendMessage(a.ID, "turn2"); err != nil {
		t.Fatalf("SendMessage turn2: %v", err)
	}
	waitForAgentState(t, a, StateRunning, testStateWaitTimeout)

	// Enqueue two follow-ups while turn2 runs: the flush should pop and
	// regate "turn3" (triggering the handoff), while "turn4" stays queued on
	// the same *Agent and must replay on copilot afterward, in order.
	if err := m.SendMessage(a.ID, "turn3"); err != nil {
		t.Fatalf("SendMessage turn3: %v", err)
	}
	if err := m.SendMessage(a.ID, "turn4"); err != nil {
		t.Fatalf("SendMessage turn4: %v", err)
	}
	if a.PendingPromptCount() != 2 {
		t.Fatalf("expected 2 prompts queued, got %d", a.PendingPromptCount())
	}

	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"claude": false, "copilot": true},
		reasons: map[string]string{"claude": "rate_limited"},
	})

	waitForAgentProvider(t, a, "copilot", testStateWaitTimeout)
	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)

	// turn4 should have replayed automatically (queue drains before the
	// per-turn loop blocks on its prompt channel), leaving the queue empty.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && a.PendingPromptCount() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if a.PendingPromptCount() != 0 {
		t.Fatalf("expected carried queue to fully drain, remaining=%d", a.PendingPromptCount())
	}

	var userTexts []string
	for _, ev := range a.ConvoOutput() {
		if ev.Type == "user_input" {
			userTexts = append(userTexts, ev.Text)
		}
	}
	want := []string{"turn2", "turn3", "turn4"}
	if len(userTexts) != len(want) {
		t.Fatalf("expected user_input events %v, got %v", want, userTexts)
	}
	for i, w := range want {
		if userTexts[i] != w {
			t.Errorf("user_input[%d] = %q, want %q (order matters)", i, userTexts[i], w)
		}
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
