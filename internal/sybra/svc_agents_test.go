package sybra

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestGetAgentRunConvoLog_ReplaysFromDisk confirms a stopped interactive
// agent's NDJSON log (Anthropic-envelope format) round-trips into
// ConvoEvents with populated text / tool_use / tool_result — the fix for
// the empty-history bug.
func TestGetAgentRunConvoLog_ReplaysFromDisk(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	logsDir := filepath.Join(home, "logs")
	tasksDir := filepath.Join(home, "tasks")
	agentsLogDir := filepath.Join(logsDir, "agents")
	if err := os.MkdirAll(agentsLogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a minimal Claude stream-json log covering the three content
	// block types the UI cares about (text / tool_use / tool_result).
	agentID := "e2e-agent-1"
	logName := agentID + "-2026-04-16T14-00-00.ndjson"
	logPath := filepath.Join(agentsLogDir, logName)
	ndjson := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"e2e rendered text"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu-1","content":"a.txt\nb.txt"}]}}`,
		`{"type":"result","subtype":"success","result":"done","session_id":"sess-1","total_cost_usd":0.01}`,
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(ndjson), 0o644); err != nil {
		t.Fatal(err)
	}

	// Task file with a single stopped interactive agent run that points at
	// the fixture log. The service falls back to agents.FindLogFile when
	// the task has no log_file set, but setting it explicitly exercises
	// the preferred path.
	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	tk, err := store.Create("history replay", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	run := task.AgentRun{
		AgentID: agentID,
		Role:    "implementation",
		Mode:    "interactive",
		State:   "stopped",
		LogFile: logPath,
	}
	if err := store.AddRun(tk.ID, run); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	svc := &AgentService{
		tasks:   taskMgr,
		logger:  quietLogger(),
		cfg:     cfg,
		logsDir: logsDir,
	}

	events, err := svc.GetAgentRunConvoLog(tk.ID, agentID)
	if err != nil {
		t.Fatalf("GetAgentRunConvoLog: %v", err)
	}

	if len(events) != 5 {
		t.Fatalf("want 5 events, got %d: %+v", len(events), events)
	}

	// Regression assertion: the assistant text must be preserved. With the
	// old code path (ParseLogFile over StreamEvent) this would be empty.
	if events[1].Type != "assistant" || events[1].Text != "e2e rendered text" {
		t.Errorf("assistant text dropped: type=%s text=%q", events[1].Type, events[1].Text)
	}

	// Tool_use block preserved.
	if len(events[2].ToolUses) != 1 || events[2].ToolUses[0].Name != "Bash" {
		t.Errorf("tool_use dropped: %+v", events[2].ToolUses)
	}

	// Tool_result preserved with pairing.
	if len(events[3].ToolResults) != 1 || events[3].ToolResults[0].ToolUseID != "tu-1" {
		t.Errorf("tool_result dropped: %+v", events[3].ToolResults)
	}
}

// TestGetAgentRunConvoLog_MissingFile returns an error rather than a silent
// empty slice so the UI can surface "failed to load log" instead of a blank
// pane with no explanation.
func TestGetAgentRunConvoLog_MissingFile(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	tasksDir := filepath.Join(home, "tasks")
	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)
	tk, err := store.Create("no log task", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	svc := &AgentService{
		tasks:   taskMgr,
		logger:  quietLogger(),
		cfg:     &config.Config{},
		logsDir: filepath.Join(home, "logs"),
	}

	_, err = svc.GetAgentRunConvoLog(tk.ID, "not-exist")
	if err == nil {
		t.Fatal("expected error for missing log file")
	}
}

// TestGetAgentRunConvoLog_UsesTaskLogFile confirms that when the task's
// agent_runs entry carries an explicit log_file path, the service reads
// from that path (even if the agent ID glob would find nothing in logsDir).
func TestGetAgentRunConvoLog_UsesTaskLogFile(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	tasksDir := filepath.Join(home, "tasks")
	// Log lives in a custom location, not under logsDir/agents/.
	customLogPath := filepath.Join(home, "custom", "my-log.ndjson")
	if err := os.MkdirAll(filepath.Dir(customLogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customLogPath, []byte(
		`{"type":"assistant","message":{"content":[{"type":"text","text":"custom path works"}]}}`+"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)
	tk, err := store.Create("custom log task", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	run := task.AgentRun{
		AgentID: "custom-agent",
		Mode:    "interactive",
		State:   "stopped",
		LogFile: customLogPath,
	}
	if err := store.AddRun(tk.ID, run); err != nil {
		t.Fatal(err)
	}

	svc := &AgentService{
		tasks:   taskMgr,
		logger:  quietLogger(),
		cfg:     &config.Config{},
		logsDir: filepath.Join(home, "logs"), // intentionally empty
	}

	events, err := svc.GetAgentRunConvoLog(tk.ID, "custom-agent")
	if err != nil {
		t.Fatalf("GetAgentRunConvoLog: %v", err)
	}
	if len(events) != 1 || events[0].Text != "custom path works" {
		t.Errorf("events = %+v, want single event with custom text", events)
	}
}
