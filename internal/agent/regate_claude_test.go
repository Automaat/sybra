package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

// TestRegateBeforeClaudeTurn_HealthyClaudeNoOp verifies that when Claude
// itself remains healthy, regateBeforeClaudeTurn never disqualifies it just
// because UsesPerTurnConvo() is false for a persistent provider — unlike
// regateForTurn, "current" here is judged on health/limit gates directly.
func TestRegateBeforeClaudeTurn_HealthyClaudeNoOp(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})
	m.mu.Lock()
	m.liveByProvider["claude"] = 1
	m.mu.Unlock()

	a := &Agent{ID: "cn1", Provider: "claude", Model: "sonnet"}
	a.SetSessionID("sess-keep")
	cfg := RunConfig{Provider: "claude", TaskID: "tcn1"}

	got, switched, err := m.regateBeforeClaudeTurn(t.Context(), a, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if switched {
		t.Fatal("expected no switch: claude is healthy")
	}
	if got.Provider != "claude" || a.Provider != "claude" {
		t.Errorf("provider changed unexpectedly: cfg=%q agent=%q", got.Provider, a.Provider)
	}
	if a.GetSessionID() != "sess-keep" {
		t.Errorf("session id must be untouched on no-op, got %q", a.GetSessionID())
	}
}

// TestRegateBeforeClaudeTurn_FailoverToHealthyPeer verifies a capped Claude
// session switches to a healthy per-turn-capable peer at the turn boundary:
// provider/model update, session clears, and the live-count bucket moves.
func TestRegateBeforeClaudeTurn_FailoverToHealthyPeer(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"claude": false, "copilot": true},
		reasons: map[string]string{"claude": "rate_limited"},
	})
	m.mu.Lock()
	m.liveByProvider["claude"] = 1
	m.mu.Unlock()

	a := &Agent{ID: "cn2", Provider: "claude", Model: "sonnet"}
	a.SetSessionID("sess-old")
	a.SetSessionFilePath("/tmp/old.jsonl")
	cfg := RunConfig{Provider: "claude", TaskID: "tcn2"}

	got, switched, err := m.regateBeforeClaudeTurn(t.Context(), a, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !switched {
		t.Fatal("expected a switch to the healthy copilot peer")
	}
	if got.Provider != "copilot" || a.Provider != "copilot" {
		t.Errorf("expected switch to copilot, cfg=%q agent=%q", got.Provider, a.Provider)
	}
	if a.GetSessionID() != "" || a.GetSessionFilePath() != "" {
		t.Errorf("session state must be cleared on switch, id=%q path=%q", a.GetSessionID(), a.GetSessionFilePath())
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if v, ok := m.liveByProvider["claude"]; ok {
		t.Errorf("claude bucket should be removed after moving its only count, got %d", v)
	}
	if m.liveByProvider["copilot"] != 1 {
		t.Errorf("copilot bucket should carry the moved count, got %+v", m.liveByProvider)
	}
}

// TestRegateBeforeClaudeTurn_NoPeerRejected verifies that when no
// per-turn-capable peer is healthy, regateBeforeClaudeTurn refuses the turn
// and records a rate_limit-compatible error kind, without ever switching
// Claude onto itself or another non-per-turn provider.
func TestRegateBeforeClaudeTurn_NoPeerRejected(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"claude": false, "codex": false, "copilot": false},
		reasons: map[string]string{"claude": "rate_limited"},
	})

	a := &Agent{ID: "cn3", Provider: "claude"}
	cfg := RunConfig{Provider: "claude", TaskID: "tcn3"}

	got, switched, err := m.regateBeforeClaudeTurn(t.Context(), a, cfg)
	if err == nil {
		t.Fatal("expected error: no healthy per-turn peer available")
	}
	if switched {
		t.Fatal("switched must be false on rejection")
	}
	if got.Provider != "claude" || a.Provider != "claude" {
		t.Errorf("provider must be unmodified on rejection, cfg=%q agent=%q", got.Provider, a.Provider)
	}
	if a.GetErrorKind() != "rate_limit" {
		t.Errorf("expected rate_limit error kind, got %q", a.GetErrorKind())
	}
}

// TestRegateBeforeClaudeTurn_SoftThresholdLastResort pins that the soft-
// threshold last resort does not require the current provider to be per-turn-
// capable. Claude never is (UsesPerTurnConvo() is false), so a check written
// against the per-turn caller alone would strand every soft-capped claude
// session — the same stranding the per-turn path suffered (#2150), just
// arriving through regateBeforeClaudeTurn instead.
func TestRegateBeforeClaudeTurn_SoftThresholdLastResort(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})
	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		DefaultProvider: "claude",
		LimitGate: &fakeLimitGate{
			available:  map[string]bool{"claude": false},
			reasons:    map[string]string{"claude": "session limit near threshold"},
			chooseNone: true,
		},
		LimitPolicy: limits.Policy{},
	}); err != nil {
		t.Fatal(err)
	}

	a := &Agent{ID: "cn4", Provider: "claude"}
	got, switched, err := m.regateBeforeClaudeTurn(t.Context(), a, RunConfig{Provider: "claude", TaskID: "tcn4"})
	if err != nil {
		t.Fatalf("soft threshold with no peer must not block claude's turn: %v", err)
	}
	if switched {
		t.Error("switched = true, want false: there is no peer to switch to")
	}
	if got.Provider != "claude" {
		t.Errorf("cfg.Provider = %q, want claude (spend the remaining budget)", got.Provider)
	}
	if a.GetErrorKind() != "" {
		t.Errorf("error kind = %q, want empty: a soft threshold is not a rate-limit failure", a.GetErrorKind())
	}
}

// fakeClaudeConvoScript is a persistent shell "claude" stand-in: each line
// read from stdin (one user message) gets one system+result reply, then it
// waits for the next line — mirroring the real CLI's persistent-session
// shape closely enough to exercise runConversational end-to-end. It exits
// cleanly on stdin EOF, matching the closeStdinPipe mechanism used both by
// the existing one-shot-close path and by the new turn-boundary handoff.
const fakeClaudeConvoScript = "#!/bin/sh\n" +
	"while IFS= read -r line; do\n" +
	"  printf '%s\\n' '{\"type\":\"system\",\"session_id\":\"claude-sess\"}'\n" +
	"  printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"claude-sess\"}'\n" +
	"done\n" +
	"exit 0\n"

// TestSendMessage_HotSwapsPersistentClaudeToPerTurnPeerAtTurnBoundary is the
// end-to-end regression test for the persistent-Claude half of mid-run
// provider failover: a Claude interactive session whose provider caps
// between turns must hot-swap to a healthy per-turn-capable peer (copilot)
// on the NEXT message, instead of writing that message to the now-capped
// Claude session's stdin.
func TestSendMessage_HotSwapsPersistentClaudeToPerTurnPeerAtTurnBoundary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(fakeClaudeConvoScript), 0o755); err != nil {
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
	a := &Agent{ID: "pconv1", TaskID: "ptask1", Provider: "claude", Mode: "interactive", sessionCWD: sessionDir, done: make(chan struct{})}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.liveByProvider["claude"] = 1
	m.liveCount = 1
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.cancel = cancel
	go m.runConversational(ctx, a, RunConfig{Prompt: "hi", Provider: "claude"})

	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)
	if a.GetProvider() != "claude" {
		t.Fatalf("expected first turn to run on claude, got %s", a.GetProvider())
	}

	// Claude caps between turns; copilot is the only healthy per-turn peer.
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"claude": false, "copilot": true},
		reasons: map[string]string{"claude": "rate_limited"},
	})

	if err := m.SendMessage(a.ID, "continue"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	waitForAgentProvider(t, a, "copilot", testStateWaitTimeout)
	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)

	if a.GetSessionID() != "cop-fake" {
		t.Errorf("expected copilot's session id captured after handoff, got %q", a.GetSessionID())
	}

	m.mu.RLock()
	claudeCount, claudeOK := m.liveByProvider["claude"]
	copilotCount := m.liveByProvider["copilot"]
	m.mu.RUnlock()
	if claudeOK && claudeCount != 0 {
		t.Errorf("claude live count should be gone after handoff, got %d", claudeCount)
	}
	if copilotCount != 1 {
		t.Errorf("copilot live count should be 1 after handoff, got %d", copilotCount)
	}

	// The conversation log now spans two schemas (claude stream-json, then
	// copilot per-turn JSON); rehydration must parse both segments.
	logPath := a.GetLogPath()
	rehydrated := &Agent{ID: "pconv1-rehydrate", Provider: "copilot"}
	rehydratePerTurnConvoFromLog(rehydrated, logPath)
	var sawClaudeResult, sawCopilotResult bool
	for _, ev := range rehydrated.ConvoOutput() {
		if ev.Type == "result" && ev.SessionID == "claude-sess" {
			sawClaudeResult = true
		}
		if ev.Type == "result" && ev.SessionID == "cop-fake" {
			sawCopilotResult = true
		}
	}
	if !sawClaudeResult {
		t.Error("expected the pre-handoff claude segment to rehydrate")
	}
	if !sawCopilotResult {
		t.Error("expected the post-handoff copilot segment to rehydrate")
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

// TestSendMessage_NoPeerBlocksWithoutWritingToCappedClaude verifies that when
// Claude caps and no healthy per-turn-capable peer exists, SendMessage
// refuses the turn (rate_limit error kind) instead of writing the follow-up
// to the doomed Claude session, and leaves the agent on Claude, still paused.
func TestSendMessage_NoPeerBlocksWithoutWritingToCappedClaude(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(fakeClaudeConvoScript), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, _ := newTestManager(t)
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"claude": true}})

	sessionDir := t.TempDir()
	a := &Agent{ID: "pconv2", TaskID: "ptask2", Provider: "claude", Mode: "interactive", sessionCWD: sessionDir, done: make(chan struct{})}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.liveByProvider["claude"] = 1
	m.liveCount = 1
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.cancel = cancel
	go m.runConversational(ctx, a, RunConfig{Prompt: "hi", Provider: "claude"})

	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)

	// Claude caps; no healthy per-turn peer at all.
	m.SetHealthGate(&fakeGate{
		healthy: map[string]bool{"claude": false, "codex": false, "copilot": false},
		reasons: map[string]string{"claude": "rate_limited"},
	})

	err := m.SendMessage(a.ID, "continue")
	if err == nil {
		t.Fatal("expected SendMessage to reject the turn when no peer is healthy")
	}
	if a.GetErrorKind() != "rate_limit" {
		t.Errorf("expected rate_limit error kind, got %q", a.GetErrorKind())
	}
	if a.GetProvider() != "claude" {
		t.Errorf("provider must be unchanged when no switch happens, got %q", a.GetProvider())
	}
	if a.GetState() != StatePaused {
		t.Errorf("expected agent to remain paused (no doomed turn spawned), got %s", a.GetState())
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
