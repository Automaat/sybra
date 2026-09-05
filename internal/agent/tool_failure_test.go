package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApprovalFailurePersistsAgentAndAggregateRecords(t *testing.T) {
	mgr := mustNewManager(t, context.Background(), func(string, any) {}, discardLogger(), t.TempDir())
	logPath := filepath.Join(mgr.logDir, "agents", "agent-1.ndjson")
	a := &Agent{
		ID:        "agent-1",
		TaskID:    "task-1",
		Mode:      "headless",
		Provider:  "claude",
		SessionID: "session-1",
		LogPath:   logPath,
		State:     StateRunning,
		StartedAt: time.Now(),
	}
	mgr.mu.Lock()
	mgr.agents[a.ID] = a
	mgr.mu.Unlock()

	srv := &ApprovalServer{agents: mgr, logger: discardLogger()}
	srv.recordApprovalFailureByID(a.ID, hookInput{
		SessionID: "session-1",
		ToolName:  "Bash",
		ToolUseID: "tool-1",
		ToolInput: map[string]any{"command": "go test ./..."},
	}, "approval-denied", "User denied this action")

	rec := readLastToolFailure(t, logPath)
	if rec.Type != toolFailureEventType || rec.Source != "approval-denied" {
		t.Fatalf("record = %+v, want approval-denied tool failure", rec)
	}
	if rec.AgentID != a.ID || rec.TaskID != a.TaskID || rec.SessionID != a.SessionID {
		t.Fatalf("identity fields = %+v", rec)
	}
	if rec.ToolName != "Bash" || rec.ToolUseID != "tool-1" || rec.ToolInputSummary != "go test ./..." {
		t.Fatalf("tool fields = %+v", rec)
	}

	agg := readLastToolFailure(t, filepath.Join(mgr.logDir, "tool-failures.ndjson"))
	if agg.ToolUseID != "tool-1" || agg.Source != "approval-denied" {
		t.Fatalf("aggregate record = %+v", agg)
	}

	events, err := ParseLogFile(logPath, 0, "claude")
	if err != nil {
		t.Fatalf("ParseLogFile: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ParseLogFile returned diagnostic events: %+v", events)
	}
}

func TestProcessHeadlessLinePersistsToolResultFailure(t *testing.T) {
	mgr := mustNewManager(t, context.Background(), func(string, any) {}, discardLogger(), t.TempDir())
	logPath := filepath.Join(mgr.logDir, "agents", "agent-2.ndjson")
	a := &Agent{
		ID:        "agent-2",
		TaskID:    "task-2",
		Mode:      "headless",
		Provider:  "claude",
		SessionID: "session-2",
		LogPath:   logPath,
		State:     StateRunning,
		StartedAt: time.Now(),
	}
	prov, err := lookupProvider("claude")
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	lastEmit := time.Now()

	mgr.processHeadlessLine(context.Background(), a, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-2","name":"Bash","input":{"command":"git diff --stat"}}]}}`), &lastEmit, prov)
	mgr.processHeadlessLine(context.Background(), a, []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-2","content":"The user doesn't want to proceed with this tool use.","is_error":true}]}}`), &lastEmit, prov)

	rec := readLastToolFailure(t, logPath)
	if rec.Source != "provider-tool-result-error" {
		t.Fatalf("Source = %q, want provider-tool-result-error", rec.Source)
	}
	if rec.ToolName != "Bash" || rec.ToolUseID != "tool-2" || rec.ToolInputSummary != "git diff --stat" {
		t.Fatalf("tool fields = %+v", rec)
	}
	if !strings.Contains(rec.Reason, "doesn't want to proceed") {
		t.Fatalf("Reason = %q", rec.Reason)
	}
}

func TestProcessHeadlessLinePersistsTerminalAbort(t *testing.T) {
	mgr := mustNewManager(t, context.Background(), func(string, any) {}, discardLogger(), t.TempDir())
	logPath := filepath.Join(mgr.logDir, "agents", "agent-3.ndjson")
	a := &Agent{
		ID:        "agent-3",
		TaskID:    "task-3",
		Mode:      "headless",
		Provider:  "claude",
		SessionID: "session-3",
		LogPath:   logPath,
		State:     StateRunning,
		StartedAt: time.Now(),
	}
	prov, err := lookupProvider("claude")
	if err != nil {
		t.Fatalf("lookupProvider: %v", err)
	}
	lastEmit := time.Now()

	mgr.processHeadlessLine(context.Background(), a, []byte(`{"type":"result","terminal_reason":"aborted_tools","result":"stopped on rejected tool"}`), &lastEmit, prov)

	rec := readLastToolFailure(t, logPath)
	if rec.Source != "stream-aborted-tools" || rec.TerminalReason != "aborted_tools" {
		t.Fatalf("record = %+v, want stream aborted_tools", rec)
	}
}

func TestRecordToolCallFailurePreservesExplicitDiagnosticFields(t *testing.T) {
	mgr := mustNewManager(t, context.Background(), func(string, any) {}, discardLogger(), t.TempDir())
	logPath := filepath.Join(mgr.logDir, "agents", "agent-4.ndjson")
	a := &Agent{
		ID:        "agent-4",
		TaskID:    "task-4",
		Mode:      "headless",
		Provider:  "claude",
		SessionID: "agent-session",
		LogPath:   logPath,
		State:     StateRunning,
		StartedAt: time.Now(),
	}
	longForeground := strings.Repeat("x", toolFailureMaxField+50)

	mgr.recordToolCallFailure(a, ToolCallFailureRecord{
		SessionID:         "observed-session",
		Provider:          "observed-provider",
		Source:            "test-source",
		ForegroundCommand: longForeground,
	})

	rec := readLastToolFailure(t, logPath)
	if rec.SessionID != "observed-session" || rec.Provider != "observed-provider" {
		t.Fatalf("record overwritten explicit fields: %+v", rec)
	}
	if rec.AgentID != a.ID || rec.TaskID != a.TaskID {
		t.Fatalf("record did not fill missing agent identity: %+v", rec)
	}
	if len(rec.ForegroundCommand) <= toolFailureMaxField {
		t.Fatalf("ForegroundCommand was not marked truncated: len=%d", len(rec.ForegroundCommand))
	}
	if !strings.HasSuffix(rec.ForegroundCommand, "...[truncated]") {
		t.Fatalf("ForegroundCommand = %q, want truncation marker", rec.ForegroundCommand)
	}
}

func readLastToolFailure(t *testing.T, path string) ToolCallFailureRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var last ToolCallFailureRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec ToolCallFailureRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if rec.Type == toolFailureEventType {
			last = rec
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if last.Type == "" {
		t.Fatalf("no %s record in %s", toolFailureEventType, path)
	}
	return last
}

func TestRecordToolCallFailure_CountsOnTheRun(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{})
	a := &Agent{ID: "a1", TaskID: "t1"}

	if got := a.GetToolFailures(); got != 0 {
		t.Fatalf("fresh run reports %d tool failures, want 0", got)
	}
	m.recordToolCallFailure(a, ToolCallFailureRecord{ToolName: "Bash", Source: "provider-tool-result-error"})
	m.recordToolCallFailure(a, ToolCallFailureRecord{ToolName: "Bash", Source: "provider-tool-result-error"})
	m.recordToolCallFailure(a, ToolCallFailureRecord{ToolName: "Read", Source: "provider-tool-result-error"})

	if got := a.GetToolFailures(); got != 3 {
		t.Errorf("tool failures = %d, want 3", got)
	}
}

func TestRecordToolCallFailure_NilAgentDoesNotPanic(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{})
	m.recordToolCallFailure(nil, ToolCallFailureRecord{ToolName: "Bash", Source: "provider-tool-result-error"})
}

func TestCountToolFailure_IsSafeUnderConcurrency(t *testing.T) {
	a := &Agent{ID: "a1"}
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(a.CountToolFailure)
	}
	wg.Wait()
	if got := a.GetToolFailures(); got != 50 {
		t.Errorf("tool failures = %d, want 50", got)
	}
}
