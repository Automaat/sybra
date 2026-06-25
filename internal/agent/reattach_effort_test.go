package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRehydrateFromLog_RestoresEffort guards the reattach/recovery path: an
// agent whose run crossed an app restart (or completed while the app was down
// and is finalized on reattach) must restore its turn and tool-call counts from
// the replayed log, not record zeros into stats.RunRecord.
func TestRehydrateFromLog_RestoresEffort(t *testing.T) {
	lines := []string{
		// Assistant turn with two tool_use blocks → +1 turn, +2 tool calls.
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls"}}]}}`,
		// Assistant text-only turn → +1 turn, +0 tool calls.
		`{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}`,
		// Result carries no turn or tool call.
		`{"type":"result","subtype":"success"}`,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.ndjson")
	data := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	a := &Agent{Provider: "claude"}
	off := rehydrateFromLog(a, path)

	if off != int64(len(data)) {
		t.Errorf("offset = %d, want %d (end of file)", off, len(data))
	}
	if got := a.GetTurnCount(); got != 2 {
		t.Errorf("TurnCount = %d, want 2", got)
	}
	if got := a.GetToolCalls(); got != 2 {
		t.Errorf("ToolCalls = %d, want 2", got)
	}
}
