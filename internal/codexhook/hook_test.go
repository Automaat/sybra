package codexhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/audit"
)

func TestMap_SessionStart(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"SessionStart","session_id":"sess-1","model":"gpt-5.5"}`)
	ev, err := Map(raw, "task-abc123", "SessionStart")
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if ev.Type != audit.EventCodexSessionStart {
		t.Errorf("Type = %q, want %q", ev.Type, audit.EventCodexSessionStart)
	}
	if ev.TaskID != "task-abc123" {
		t.Errorf("TaskID = %q, want task-abc123", ev.TaskID)
	}
	if ev.Data["session_id"] != "sess-1" {
		t.Errorf("session_id = %v", ev.Data["session_id"])
	}
	if ev.Data["model"] != "gpt-5.5" {
		t.Errorf("model = %v", ev.Data["model"])
	}
	if ev.Timestamp.IsZero() {
		t.Error("Timestamp is zero")
	}
}

func TestMap_SubagentStart(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"SubagentStart","session_id":"s","subagent_id":"sa-1","kind":"coder","model":"gpt-5.5"}`)
	ev, err := Map(raw, "task-xyz", "SubagentStart")
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if ev.Type != audit.EventCodexSubagentStart {
		t.Errorf("Type = %q, want %q", ev.Type, audit.EventCodexSubagentStart)
	}
	if ev.Data["subagent_id"] != "sa-1" {
		t.Errorf("subagent_id = %v", ev.Data["subagent_id"])
	}
	if ev.Data["kind"] != "coder" {
		t.Errorf("kind = %v", ev.Data["kind"])
	}
}

func TestMap_SubagentStop(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"SubagentStop","session_id":"s","subagent_id":"sa-1","kind":"coder"}`)
	ev, err := Map(raw, "t", "SubagentStop")
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if ev.Type != audit.EventCodexSubagentStop {
		t.Errorf("Type = %q", ev.Type)
	}
}

func TestMap_Stop(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"Stop","session_id":"s","model":"gpt-5.5"}`)
	ev, err := Map(raw, "t", "Stop")
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if ev.Type != audit.EventCodexSessionStop {
		t.Errorf("Type = %q", ev.Type)
	}
}

func TestMap_UnknownEvent(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"PreToolUse","session_id":"s"}`)
	_, err := Map(raw, "t", "PreToolUse")
	if err == nil {
		t.Fatal("expected error for unknown event")
	}
	if !strings.Contains(err.Error(), "PreToolUse") {
		t.Errorf("error should mention the unknown event name; got %v", err)
	}
}

func TestMap_EventMismatch(t *testing.T) {
	t.Parallel()
	// Positional event "Stop" but payload says "SessionStart" — must error.
	raw := []byte(`{"hook_event_name":"SessionStart","session_id":"s-mismatch","model":"m"}`)
	_, err := Map(raw, "t", "Stop")
	if err == nil {
		t.Fatal("expected error for event name mismatch")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error should mention mismatch; got %v", err)
	}
}

func TestMap_MalformedJSON(t *testing.T) {
	t.Parallel()
	_, err := Map([]byte("{not json"), "t", "SessionStart")
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestMap_NoForbiddenFields asserts that the returned audit event Data contains
// only structural-identity fields and never carries sensitive payload content.
func TestMap_NoForbiddenFields(t *testing.T) {
	t.Parallel()
	// Payload with extra fields that must be silently dropped.
	raw := []byte(`{
		"hook_event_name": "SessionStart",
		"session_id": "sess-1",
		"model": "gpt-5.5",
		"cwd": "/home/user/secret",
		"transcript_path": "/tmp/sess.jsonl",
		"tool_input": {"cmd": "rm -rf /"},
		"prompt": "secret prompt text",
		"command": "rm -rf /"
	}`)
	ev, err := Map(raw, "task-1", "SessionStart")
	if err != nil {
		t.Fatalf("Map: %v", err)
	}

	forbidden := []string{"cwd", "transcript_path", "tool_input", "prompt", "command"}
	for _, key := range forbidden {
		if _, ok := ev.Data[key]; ok {
			t.Errorf("Data must not contain %q; got %v", key, ev.Data)
		}
	}
}

// TestMap_LineSizeUnder4096 asserts that the marshaled audit event from a
// maximal structural payload stays well under 4096 bytes.
func TestMap_LineSizeUnder4096(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"hook_event_name":"SubagentStart","session_id":"sess-aaaaaaaaaaaaaaaaaaaaaaaaa","model":"gpt-5.5-turbo-xxxx","subagent_id":"subagent-bbbbbbbbbbbbbb","kind":"coder-specialist"}`)
	ev, err := Map(raw, "task-cccccccccccccccccccc", "SubagentStart")
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(line) >= 4096 {
		t.Errorf("marshaled event is %d bytes, must be < 4096; line=%s", len(line), line)
	}
}
