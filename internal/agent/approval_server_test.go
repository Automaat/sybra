package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNewApprovalServer_PinnedPort verifies the approval server binds the
// configured port (so a detached agent's baked hook URL survives restart),
// and that 0 binds a random port.
func TestNewApprovalServer_PinnedPort(t *testing.T) {
	// Find a currently-free port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	srv, err := NewApprovalServer(context.Background(), func(_ string, _ any) {}, discardLogger(), port)
	if err != nil {
		t.Fatalf("NewApprovalServer pinned: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	if !strings.HasSuffix(srv.Addr(), ":"+strconv.Itoa(port)) {
		t.Fatalf("addr %q does not use pinned port %d", srv.Addr(), port)
	}

	rnd, err := NewApprovalServer(context.Background(), func(_ string, _ any) {}, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewApprovalServer random: %v", err)
	}
	t.Cleanup(func() { _ = rnd.Shutdown(context.Background()) })
	if rnd.Addr() == "" {
		t.Fatal("random-port server has empty addr")
	}
}

func newTestApprovalServer(t *testing.T) *ApprovalServer {
	t.Helper()
	srv, err := NewApprovalServer(context.Background(), func(_ string, _ any) {}, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewApprovalServer: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
	})
	return srv
}

func postHook(t *testing.T, addr string, body map[string]any) *hookResponse {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post("http://"+addr+"/hooks/pre-tool-use", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	var out hookResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &out
}

func TestNewApprovalServer(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)
	if srv.Addr() == "" {
		t.Error("expected non-empty addr")
	}
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Errorf("addr %q does not start with 127.0.0.1:", srv.Addr())
	}
}

func TestApprovalServer_SafeToolAutoApprove(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)

	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "any-session",
		"tool_name":   "Read",
		"tool_use_id": "tuid-1",
		"tool_input":  map[string]any{},
	})
	if resp.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("expected allow, got %q", resp.HookSpecificOutput.PermissionDecision)
	}
}

// TestApprovalServer_MCPNotAutoApproved verifies MCP tools are never
// blanket-auto-approved: with no manager/agent set up, an mcp__ tool falls
// through to the unknown-session deny path rather than short-circuiting to
// allow like the old isSafeTool blanket "mcp__*" match did.
func TestApprovalServer_MCPNotAutoApproved(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)

	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "any-session",
		"tool_name":   "mcp__fs__read",
		"tool_use_id": "tuid-mcp",
		"tool_input":  map[string]any{},
	})
	if resp.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected deny for mcp__ tool (no blanket auto-approve), got %q", resp.HookSpecificOutput.PermissionDecision)
	}
}

// TestApprovalServer_WebFetchNotAutoApproved verifies WebFetch requires
// approval like any other egress-sensitive tool, since it is a ready
// exfiltration channel for content ingested from untrusted sources.
func TestApprovalServer_WebFetchNotAutoApproved(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)

	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "any-session",
		"tool_name":   "WebFetch",
		"tool_use_id": "tuid-webfetch",
		"tool_input":  map[string]any{},
	})
	if resp.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected deny for WebFetch (no blanket auto-approve), got %q", resp.HookSpecificOutput.PermissionDecision)
	}
}

func TestApprovalServer_UnknownSession(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)

	// No manager set → unknown session → deny (fail-closed).
	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "unknown-session",
		"tool_name":   "Bash",
		"tool_use_id": "tuid-unknown",
		"tool_input":  map[string]any{},
	})
	if resp.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected deny for unknown session, got %q", resp.HookSpecificOutput.PermissionDecision)
	}
}

// TestFindAgentBySession_MatchesHeadlessAgent verifies the approval hook
// resolves a headless-mode agent by session ID, not just interactive ones.
// Headless runs only reach this hook when RequirePermissions is set (see
// buildClaudeHookSettings), so restricting the match to Mode=="interactive"
// left every headless approval request unresolved and fail-closed to deny —
// forcing operators to disable permission requirements entirely.
func TestFindAgentBySession_MatchesHeadlessAgent(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t)
	fakeAgent := &Agent{
		ID:        "fake-headless-1",
		Mode:      "headless",
		SessionID: "session-headless",
		State:     StateRunning,
	}
	mgr.mu.Lock()
	mgr.agents["fake-headless-1"] = fakeAgent
	mgr.mu.Unlock()

	srv := &ApprovalServer{agents: mgr}
	if got := srv.findAgentBySession("session-headless"); got != "fake-headless-1" {
		t.Errorf("findAgentBySession(headless) = %q, want %q", got, "fake-headless-1")
	}
}

// TestFindAgentBySession_PrefersLiveRetryOverStaleOriginal covers the
// registry-never-pruned hazard: a rate-limited retry resumes the prior session
// ID, so the stopped original and the live retry share a SessionID. The lookup
// must resolve to the live retry, not the dead original nobody is watching.
func TestFindAgentBySession_PrefersLiveRetryOverStaleOriginal(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t)
	base := time.Now()
	original := &Agent{
		ID:        "orig",
		Mode:      "headless",
		SessionID: "shared-session",
		State:     StateStopped,
		StartedAt: base,
	}
	retry := &Agent{
		ID:        "retry",
		Mode:      "headless",
		SessionID: "shared-session",
		State:     StateRunning,
		StartedAt: base.Add(time.Minute),
	}
	mgr.mu.Lock()
	mgr.agents["orig"] = original
	mgr.agents["retry"] = retry
	mgr.mu.Unlock()

	srv := &ApprovalServer{agents: mgr}
	if got := srv.findAgentBySession("shared-session"); got != "retry" {
		t.Errorf("findAgentBySession = %q, want %q (live retry)", got, "retry")
	}
}

// TestFindAgentBySession_MostRecentAmongSameLiveness verifies the tie-breaker
// when two matching agents share liveness: the most-recently-started wins.
func TestFindAgentBySession_MostRecentAmongSameLiveness(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t)
	base := time.Now()
	older := &Agent{
		ID:        "older",
		Mode:      "headless",
		SessionID: "same-session",
		State:     StateRunning,
		StartedAt: base,
	}
	newer := &Agent{
		ID:        "newer",
		Mode:      "headless",
		SessionID: "same-session",
		State:     StateRunning,
		StartedAt: base.Add(time.Second),
	}
	mgr.mu.Lock()
	mgr.agents["older"] = older
	mgr.agents["newer"] = newer
	mgr.mu.Unlock()

	srv := &ApprovalServer{agents: mgr}
	if got := srv.findAgentBySession("same-session"); got != "newer" {
		t.Errorf("findAgentBySession = %q, want %q (most recent)", got, "newer")
	}
}

func TestFindAgentBySession_DeterministicTieBreakOnAgentID(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t)
	started := time.Now()
	low := &Agent{
		ID:        "agent-a",
		Mode:      "headless",
		SessionID: "same-session",
		State:     StateRunning,
		StartedAt: started,
	}
	high := &Agent{
		ID:        "agent-b",
		Mode:      "headless",
		SessionID: "same-session",
		State:     StateRunning,
		StartedAt: started,
	}
	mgr.mu.Lock()
	mgr.agents[low.ID] = low
	mgr.agents[high.ID] = high
	mgr.mu.Unlock()

	srv := &ApprovalServer{agents: mgr}
	if got := srv.findAgentBySession("same-session"); got != "agent-b" {
		t.Errorf("findAgentBySession = %q, want %q (deterministic agent ID tie-break)", got, "agent-b")
	}
}

func TestFindAgentBySessionWithRetry_EmptySessionReturnsImmediately(t *testing.T) {
	t.Parallel()

	srv := &ApprovalServer{}
	start := time.Now()
	if got := srv.findAgentBySessionWithRetry(context.Background(), " \t "); got != "" {
		t.Fatalf("findAgentBySessionWithRetry(empty) = %q, want empty", got)
	}
	if elapsed := time.Since(start); elapsed >= sessionLookupBackoff {
		t.Fatalf("findAgentBySessionWithRetry(empty) took %v, want < %v", elapsed, sessionLookupBackoff)
	}
}
func TestApprovalServer_CanceledContext(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t)
	fakeAgent := &Agent{
		ID:        "fake-ag-cancel",
		Mode:      "interactive",
		SessionID: "session-cancel",
		State:     StateRunning,
	}
	mgr.mu.Lock()
	mgr.agents["fake-ag-cancel"] = fakeAgent
	mgr.mu.Unlock()

	srv, err := NewApprovalServer(context.Background(), func(_ string, _ any) {}, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewApprovalServer: %v", err)
	}
	srv.SetManager(mgr)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{
		"session_id":  "session-cancel",
		"tool_name":   "Bash",
		"tool_use_id": "tuid-cancel",
		"tool_input":  map[string]any{},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/hooks/pre-tool-use", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Cancel once the handler has registered the pending approval (i.e. is
	// actually blocked waiting), instead of guessing a sleep duration.
	registered := make(chan bool, 1)
	go func() {
		ok := pollUntil(2*time.Second, time.Millisecond, func() bool {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			_, ok := srv.pending["tuid-cancel"]
			return ok
		})
		registered <- ok
		cancel()
	}()

	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.handlePreToolUse(rr, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler blocked after context cancellation")
	}

	if !<-registered {
		t.Errorf("pending approval %q was not registered before cancellation", "tuid-cancel")
	}

	var out hookResponse
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected deny on canceled context, got %q", out.HookSpecificOutput.PermissionDecision)
	}

	// Agent state must be restored after cancellation.
	if fakeAgent.GetState() != StateRunning {
		t.Errorf("expected state %q after cancel, got %q", StateRunning, fakeAgent.GetState())
	}
	fakeAgent.mu.RLock()
	awaitingApproval := fakeAgent.AwaitingApproval
	fakeAgent.mu.RUnlock()
	if awaitingApproval {
		t.Error("expected AwaitingApproval=false after canceled context")
	}
}

func TestApprovalServer_ApprovalFlow_Approve(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t)
	// Inject a fake interactive agent so findAgentBySession returns its ID.
	fakeAgent := &Agent{
		ID:        "fake-ag-1",
		Mode:      "interactive",
		SessionID: "session-approve",
		State:     StateRunning,
	}
	mgr.mu.Lock()
	mgr.agents["fake-ag-1"] = fakeAgent
	mgr.mu.Unlock()

	srv, err := NewApprovalServer(context.Background(), func(_ string, _ any) {}, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewApprovalServer: %v", err)
	}
	srv.SetManager(mgr)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	// Respond approve once the pending item is actually registered, instead
	// of guessing how long registration takes.
	registered := make(chan bool, 1)
	go func() {
		ok := pollUntil(2*time.Second, time.Millisecond, func() bool {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			_, ok := srv.pending["tuid-approve"]
			return ok
		})
		if ok {
			_ = srv.RespondApproval("tuid-approve", true)
		}
		registered <- ok
	}()

	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "session-approve",
		"tool_name":   "Bash",
		"tool_use_id": "tuid-approve",
		"tool_input":  map[string]any{"command": "echo hi"},
	})

	if !<-registered {
		t.Errorf("pending approval %q was not registered before approving", "tuid-approve")
	}
	if resp.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("expected allow after approve, got %q", resp.HookSpecificOutput.PermissionDecision)
	}
}

func TestApprovalServer_ApprovalFlow_Deny(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t)
	fakeAgent := &Agent{
		ID:        "fake-ag-2",
		Mode:      "interactive",
		SessionID: "session-deny",
		State:     StateRunning,
	}
	mgr.mu.Lock()
	mgr.agents["fake-ag-2"] = fakeAgent
	mgr.mu.Unlock()

	srv, err := NewApprovalServer(context.Background(), func(_ string, _ any) {}, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewApprovalServer: %v", err)
	}
	srv.SetManager(mgr)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	registered := make(chan bool, 1)
	go func() {
		ok := pollUntil(2*time.Second, time.Millisecond, func() bool {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			_, ok := srv.pending["tuid-deny"]
			return ok
		})
		if ok {
			_ = srv.RespondApproval("tuid-deny", false)
		}
		registered <- ok
	}()

	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "session-deny",
		"tool_name":   "Write",
		"tool_use_id": "tuid-deny",
		"tool_input":  map[string]any{"file_path": "/tmp/x"},
	})

	if !<-registered {
		t.Errorf("pending approval %q was not registered before denying", "tuid-deny")
	}
	if resp.HookSpecificOutput.PermissionDecision != "deny" {
		t.Errorf("expected deny, got %q", resp.HookSpecificOutput.PermissionDecision)
	}
}

func TestApprovalServer_RespondApproval_NoPending(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)
	err := srv.RespondApproval("nonexistent-tuid", true)
	if err == nil {
		t.Fatal("expected error for non-existent pending approval")
	}
}

// TestApprovalServer_RespondApproval_DoubleSendDoesNotBlock simulates a UI
// double-click race: the user clicks Approve twice, the handler reads the
// first response and returns, but the deferred `delete(pending, id)` hasn't
// run yet. The second send must NOT block the UI thread on a full buffered
// channel — non-blocking send returns a clear "already consumed" error.
// A regression that reverted the select+default to a plain `ch <- ...` would
// hang the test for the full 500ms deadline.
func TestApprovalServer_RespondApproval_DoubleSendDoesNotBlock(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)

	// Manually plant a pending entry — bypasses needing a real agent flow.
	ch := make(chan ApprovalResponse, 1)
	srv.mu.Lock()
	srv.pending["double-click"] = ch
	srv.mu.Unlock()

	// First send fills the buffer.
	if err := srv.RespondApproval("double-click", true); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Second send must not block (buffer is full, no reader yet).
	done := make(chan error, 1)
	go func() { done <- srv.RespondApproval("double-click", true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("second send returned nil; expected 'already consumed' error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second RespondApproval blocked — UI thread would hang on a double-click race")
	}
}

func TestIsSafeTool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"Read", true},
		{"Glob", true},
		{"Grep", true},
		{"LSP", true},
		{"WebSearch", true},
		{"WebFetch", false},
		{"mcp__anything", false},
		{"mcp__fs__read", false},
		{"Bash", false},
		{"Edit", false},
		{"Write", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isSafeTool(tt.name)
			if got != tt.want {
				t.Errorf("isSafeTool(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
