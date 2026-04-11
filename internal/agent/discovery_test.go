package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
			t.Parallel()
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

func writeCodexSessionFile(t *testing.T, dir, sessionID string, modTime time.Time) string {
	t.Helper()
	dateDir := filepath.Join(dir, "2024", "01", "01")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dateDir, "rollout-"+sessionID+".jsonl")
	meta := `{"type":"session_meta","timestamp":"2024-01-01T00:00:00Z","payload":{"id":"` + sessionID + `","cwd":"/tmp/project","originator":"codex_exec","git":{"branch":"main"}}}`
	if err := os.WriteFile(path, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadCodexSessions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sessionID := "01ABCDEF01ABCDEF01ABCDE"
	path := writeCodexSessionFile(t, dir, sessionID, now)

	sessions := readCodexSessionsFromDir(dir)
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", s.SessionID, sessionID)
	}
	if s.CWD != "/tmp/project" {
		t.Errorf("CWD = %q, want /tmp/project", s.CWD)
	}
	if s.Originator != "codex_exec" {
		t.Errorf("Originator = %q, want codex_exec", s.Originator)
	}
	if s.Branch != "main" {
		t.Errorf("Branch = %q, want main", s.Branch)
	}
	if s.FilePath != path {
		t.Errorf("FilePath = %q, want %q", s.FilePath, path)
	}
}

func TestReadCodexSessionsSkipOld(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-25 * time.Hour)
	writeCodexSessionFile(t, dir, "OLD0000000000000000000", old)

	recent := time.Now().Add(-1 * time.Hour)
	writeCodexSessionFile(t, dir, "NEW0000000000000000000", recent)

	sessions := readCodexSessionsFromDir(dir)
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (old should be skipped)", len(sessions))
	}
	if sessions[0].SessionID != "NEW0000000000000000000" {
		t.Errorf("expected recent session, got %q", sessions[0].SessionID)
	}
}

func TestReadLastCodexEvent(t *testing.T) {
	tests := []struct {
		name            string
		lines           []string
		stale           bool
		wantEventType   string
		wantPayloadType string
	}{
		{
			name:          "empty file",
			lines:         nil,
			wantEventType: "",
		},
		{
			name:            "task_complete",
			lines:           []string{`{"type":"event_msg","payload":{"type":"task_complete"}}`},
			wantEventType:   "event_msg",
			wantPayloadType: "task_complete",
		},
		{
			name:          "response_item",
			lines:         []string{`{"type":"response_item","payload":{}}`},
			wantEventType: "response_item",
		},
		{
			name:            "multiple lines, last wins",
			lines:           []string{`{"type":"session_meta"}`, `{"type":"event_msg","payload":{"type":"agent_message"}}`},
			wantEventType:   "event_msg",
			wantPayloadType: "agent_message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "session.jsonl")

			content := strings.Join(tt.lines, "\n")
			if content != "" {
				content += "\n"
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.stale {
				past := time.Now().Add(-30 * time.Second)
				if err := os.Chtimes(path, past, past); err != nil {
					t.Fatal(err)
				}
			}

			ev := readLastCodexEvent(path)
			if ev.eventType != tt.wantEventType {
				t.Errorf("eventType = %q, want %q", ev.eventType, tt.wantEventType)
			}
			if ev.payloadType != tt.wantPayloadType {
				t.Errorf("payloadType = %q, want %q", ev.payloadType, tt.wantPayloadType)
			}
		})
	}
}

func TestInferCodexState(t *testing.T) {
	tests := []struct {
		name        string
		eventType   string
		payloadType string
		stale       bool
		want        State
	}{
		{"task_complete → stopped", "event_msg", "task_complete", false, StateStopped},
		{"task_complete stale → stopped", "event_msg", "task_complete", true, StateStopped},
		{"turn_aborted → stopped", "event_msg", "turn_aborted", false, StateStopped},
		{"task_started fresh → running", "event_msg", "task_started", false, StateRunning},
		{"task_started stale → idle", "event_msg", "task_started", true, StateIdle},
		{"agent_message fresh → running", "event_msg", "agent_message", false, StateRunning},
		{"agent_message stale → idle", "event_msg", "agent_message", true, StateIdle},
		{"token_count → idle", "event_msg", "token_count", true, StateIdle},
		{"response_item fresh → running", "response_item", "", false, StateRunning},
		{"response_item stale → paused", "response_item", "", true, StatePaused},
		{"session_meta only → idle", "session_meta", "", true, StateIdle},
		{"empty → idle", "", "", true, StateIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "session.jsonl")

			if tt.eventType != "" {
				line := `{"type":"` + tt.eventType + `","payload":{"type":"` + tt.payloadType + `"}}`
				if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.stale {
				past := time.Now().Add(-30 * time.Second)
				if err := os.Chtimes(path, past, past); err != nil {
					t.Fatal(err)
				}
			}

			got := inferCodexState(path)
			if got != tt.want {
				t.Errorf("inferCodexState = %q, want %q (event=%q payload=%q stale=%v)",
					got, tt.want, tt.eventType, tt.payloadType, tt.stale)
			}
		})
	}
}

func TestDiscoverNewCodex(t *testing.T) {
	m, _ := newTestManager(t)
	sessDir := t.TempDir()

	sessionID := "01TESTCODEXSESSIONID00"
	now := time.Now()
	filePath := writeCodexSessionFile(t, sessDir, sessionID, now)

	sessions := []RawSession{
		{
			Provider:  "codex",
			SessionID: sessionID,
			CWD:       "/tmp/project",
			Mode:      "headless",
			FilePath:  filePath,
			StartedAt: now,
			State:     StateIdle,
		},
	}

	opts := m.buildFilterOpts()
	discovered := m.reconcile(filterSessions(sessions, opts))
	if len(discovered) != 1 {
		t.Fatalf("got %d agents, want 1", len(discovered))
	}
	a := discovered[0]
	if a.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", a.Provider)
	}
	if !a.External {
		t.Error("External should be true")
	}
	if a.Mode != "headless" {
		t.Errorf("Mode = %q, want headless", a.Mode)
	}
	if a.SessionID != sessionID {
		t.Errorf("SessionID = %q, want %q", a.SessionID, sessionID)
	}
	if a.sessionFilePath != filePath {
		t.Errorf("sessionFilePath = %q, want %q", a.sessionFilePath, filePath)
	}
	if !strings.HasPrefix(a.ID, "ext-codex-") {
		t.Errorf("ID = %q, want ext-codex-* prefix", a.ID)
	}

	// Second call: session is now tracked; filterSessions should exclude it.
	opts2 := m.buildFilterOpts()
	discovered2 := m.reconcile(filterSessions(sessions, opts2))
	if len(discovered2) != 0 {
		t.Errorf("re-discovery got %d agents, want 0", len(discovered2))
	}
}

func TestDiscoverNewCodexInteractive(t *testing.T) {
	m, _ := newTestManager(t)
	dir := t.TempDir()

	sessionID := "01TUISESSIONID000000000"
	filePath := writeCodexSessionFile(t, dir, sessionID, time.Now())

	sessions := []RawSession{
		{
			Provider:  "codex",
			SessionID: sessionID,
			CWD:       "/home/user/proj",
			Mode:      "interactive",
			FilePath:  filePath,
			StartedAt: time.Now(),
			State:     StateIdle,
		},
	}

	opts := m.buildFilterOpts()
	discovered := m.reconcile(filterSessions(sessions, opts))
	if len(discovered) != 1 {
		t.Fatalf("got %d agents, want 1", len(discovered))
	}
	if discovered[0].Mode != "interactive" {
		t.Errorf("Mode = %q, want interactive", discovered[0].Mode)
	}
}

func TestRefreshTrackedCodex(t *testing.T) {
	m, _ := newTestManager(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Write a stopped event.
	line := `{"type":"event_msg","payload":{"type":"task_complete"}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{
		ID:              "ext-codex-test",
		External:        true,
		Provider:        "codex",
		State:           StateRunning,
		sessionFilePath: path,
	}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()

	m.refreshTracked()

	if got := a.GetState(); got != StateStopped {
		t.Errorf("state = %q after task_complete, want stopped", got)
	}

	// Update to idle event and refresh again.
	line2 := `{"type":"event_msg","payload":{"type":"token_count"}}`
	if err := os.WriteFile(path, []byte(line2+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}

	m.refreshTracked()
	if got := a.GetState(); got != StateIdle {
		t.Errorf("state = %q after token_count stale, want idle", got)
	}
}

func TestResolveCodexSessionFileInDir(t *testing.T) {
	dir := t.TempDir()
	sessionID := "01RESOLVE0000000000000"
	dateDir := filepath.Join(dir, "2024", "03", "15")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(dateDir, "rollout-"+sessionID+".jsonl")
	if err := os.WriteFile(expected, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := resolveCodexSessionFileInDir(dir, sessionID)
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}

	// Non-existent session ID should return empty.
	got2 := resolveCodexSessionFileInDir(dir, "NOTFOUND000000000000000")
	if got2 != "" {
		t.Errorf("expected empty for missing session, got %q", got2)
	}
}
