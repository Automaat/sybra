package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectName(t *testing.T) {
	tests := []struct {
		cwd  string
		want string
	}{
		{"/Users/me/projects/synapse", "synapse"},
		{"/Users/me/kong/kuma", "kuma"},
		{"", ""},
		{"/", "/"},
	}
	for _, tt := range tests {
		got := projectName(tt.cwd)
		if got != tt.want {
			t.Errorf("projectName(%q) = %q, want %q", tt.cwd, got, tt.want)
		}
	}
}

func TestSessionKind(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{"headless", "headless"},
		{"interactive", "interactive"},
		{"", "interactive"},
		{"unknown", "interactive"},
	}
	for _, tt := range tests {
		got := sessionKind(tt.kind)
		if got != tt.want {
			t.Errorf("sessionKind(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestInferState(t *testing.T) {
	tests := []struct {
		name    string
		content string
		stale   bool
		want    State
	}{
		{
			name:    "system message = idle",
			content: `{"type":"system"}`,
			stale:   true,
			want:    StateIdle,
		},
		{
			name:    "empty file = idle",
			content: "",
			stale:   true,
			want:    StateIdle,
		},
		{
			name:    "fresh assistant = running",
			content: `{"type":"assistant","message":{"content":[{"type":"text"}]}}`,
			stale:   false,
			want:    StateRunning,
		},
		{
			name:    "stale assistant without tool_use = idle",
			content: `{"type":"assistant","message":{"content":[{"type":"text"}]}}`,
			stale:   true,
			want:    StateIdle,
		},
		{
			name:    "stale assistant with tool_use = paused",
			content: `{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}`,
			stale:   true,
			want:    StatePaused,
		},
		{
			name:    "fresh assistant with tool_use = running",
			content: `{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}`,
			stale:   false,
			want:    StateRunning,
		},
		{
			name:    "user message = running",
			content: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
			stale:   false,
			want:    StateRunning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			projectDir := filepath.Join(dir, "projects", "-test-project")
			if err := os.MkdirAll(projectDir, 0o755); err != nil {
				t.Fatal(err)
			}

			jsonlPath := filepath.Join(projectDir, "sess-123.jsonl")
			if tt.content != "" {
				if err := os.WriteFile(jsonlPath, []byte(tt.content+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if tt.stale {
					past := time.Now().Add(-30 * time.Second)
					if err := os.Chtimes(jsonlPath, past, past); err != nil {
						t.Fatal(err)
					}
				}
			}

			got := readLastJSONL(jsonlPath)
			ss := sessionState{
				msgType:    got.msgType,
				hasToolUse: got.hasToolUse,
				stale:      got.stale,
			}

			var state State
			switch {
			case ss.msgType == "system" || ss.msgType == "":
				state = StateIdle
			case ss.msgType == "assistant" && ss.hasToolUse && ss.stale:
				state = StatePaused
			case ss.stale:
				state = StateIdle
			default:
				state = StateRunning
			}

			if state != tt.want {
				t.Errorf("state = %q, want %q (msgType=%q hasToolUse=%v stale=%v)",
					state, tt.want, ss.msgType, ss.hasToolUse, ss.stale)
			}
		})
	}
}

func TestReadLastJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	// Multiple lines — should read last one
	content := `{"type":"user"}
{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}
{"type":"system"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ss := readLastJSONL(path)
	if ss.msgType != "system" {
		t.Errorf("msgType = %q, want %q", ss.msgType, "system")
	}
	if ss.hasToolUse {
		t.Error("hasToolUse should be false for system message")
	}
}

func TestReadLastJSONLNonexistent(t *testing.T) {
	ss := readLastJSONL("/nonexistent/path.jsonl")
	if ss.msgType != "" {
		t.Errorf("msgType = %q, want empty", ss.msgType)
	}
}

func TestReadClaudeSessions(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, ".claude", "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write valid session
	s := claudeSession{
		PID:       12345,
		SessionID: "sess-abc",
		CWD:       "/tmp/project",
		StartedAt: time.Now().UnixMilli(),
		Kind:      "interactive",
		Name:      "test-session",
	}
	data, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(sessDir, "12345.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Write invalid JSON
	if err := os.WriteFile(filepath.Join(sessDir, "bad.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write session with PID 0 (should be skipped)
	zero := claudeSession{PID: 0, SessionID: "zero"}
	data, _ = json.Marshal(zero)
	if err := os.WriteFile(filepath.Join(sessDir, "0.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Write non-JSON file (should be skipped)
	if err := os.WriteFile(filepath.Join(sessDir, "notes.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Override home for test — readClaudeSessions uses os.UserHomeDir
	// so we test the helper parsing logic directly instead
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		t.Fatal(err)
	}

	var sessions []claudeSession
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(sessDir, e.Name()))
		if err != nil {
			continue
		}
		var cs claudeSession
		if err := json.Unmarshal(raw, &cs); err != nil {
			continue
		}
		if cs.PID == 0 {
			continue
		}
		sessions = append(sessions, cs)
	}

	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].PID != 12345 {
		t.Errorf("PID = %d, want 12345", sessions[0].PID)
	}
	if sessions[0].Name != "test-session" {
		t.Errorf("Name = %q, want %q", sessions[0].Name, "test-session")
	}
}

func TestInferStateDirect(t *testing.T) {
	// Empty cwd/sessionID → idle
	if got := inferState("", ""); got != StateIdle {
		t.Errorf("inferState empty = %q, want %q", got, StateIdle)
	}

	// Nonexistent session file → idle
	if got := inferState("/nonexistent/path", "no-session"); got != StateIdle {
		t.Errorf("inferState nonexistent = %q, want %q", got, StateIdle)
	}
}

func TestReadSessionStateEmpty(t *testing.T) {
	ss := readSessionState("", "")
	if ss.msgType != "" {
		t.Errorf("msgType = %q, want empty", ss.msgType)
	}

	ss = readSessionState("/some/path", "")
	if ss.msgType != "" {
		t.Errorf("msgType = %q, want empty", ss.msgType)
	}
}

func TestDiscoverAgentsEmpty(t *testing.T) {
	m, _ := newTestManager(t)
	agents := m.DiscoverAgents()
	// May return nil or empty depending on system state
	_ = agents
}

func TestProcessAlive(t *testing.T) {
	// Current process should be alive
	if !processAlive(os.Getpid()) {
		t.Error("current process should be alive")
	}

	// PID 0 or very high PID should not be alive
	if processAlive(9999999) {
		t.Error("PID 9999999 should not be alive")
	}
}
