package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestApprovalServer(t *testing.T) *ApprovalServer {
	t.Helper()
	srv, err := NewApprovalServer(func(_ string, _ any) {}, discardLogger())
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

func TestApprovalServer_SafeToolMCP(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)

	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "any-session",
		"tool_name":   "mcp__fs__read",
		"tool_use_id": "tuid-mcp",
		"tool_input":  map[string]any{},
	})
	if resp.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("expected allow for mcp__ tool, got %q", resp.HookSpecificOutput.PermissionDecision)
	}
}

func TestApprovalServer_UnknownSession(t *testing.T) {
	t.Parallel()
	srv := newTestApprovalServer(t)

	// No manager set → unknown session → auto-allow.
	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "unknown-session",
		"tool_name":   "Bash",
		"tool_use_id": "tuid-unknown",
		"tool_input":  map[string]any{},
	})
	if resp.HookSpecificOutput.PermissionDecision != "allow" {
		t.Errorf("expected allow for unknown session, got %q", resp.HookSpecificOutput.PermissionDecision)
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

	srv, err := NewApprovalServer(func(_ string, _ any) {}, discardLogger())
	if err != nil {
		t.Fatalf("NewApprovalServer: %v", err)
	}
	srv.SetManager(mgr)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	// Respond approve concurrently after the pending item is registered.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = srv.RespondApproval("tuid-approve", true)
	}()

	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "session-approve",
		"tool_name":   "Bash",
		"tool_use_id": "tuid-approve",
		"tool_input":  map[string]any{"command": "echo hi"},
	})
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

	srv, err := NewApprovalServer(func(_ string, _ any) {}, discardLogger())
	if err != nil {
		t.Fatalf("NewApprovalServer: %v", err)
	}
	srv.SetManager(mgr)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = srv.RespondApproval("tuid-deny", false)
	}()

	resp := postHook(t, srv.Addr(), map[string]any{
		"session_id":  "session-deny",
		"tool_name":   "Write",
		"tool_use_id": "tuid-deny",
		"tool_input":  map[string]any{"file_path": "/tmp/x"},
	})
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
		{"WebFetch", true},
		{"mcp__anything", true},
		{"mcp__fs__read", true},
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
