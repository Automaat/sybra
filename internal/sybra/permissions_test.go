package sybra

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

func TestResolveHeadlessPermissionMode(t *testing.T) {
	t.Parallel()

	cfgAuto := &config.Config{Agent: config.AgentDefaults{HeadlessPermissionMode: "auto"}}
	cfgBypass := &config.Config{Agent: config.AgentDefaults{HeadlessPermissionMode: "bypass"}}
	cfgEmpty := &config.Config{}

	cases := []struct {
		name    string
		t       task.Task
		cfg     *config.Config
		want    string
		wantErr bool
	}{
		{
			name: "task auto overrides config bypass",
			t:    task.Task{ID: "t1", HeadlessPermissionMode: "auto"},
			cfg:  cfgBypass,
			want: "auto",
		},
		{
			name: "task bypass overrides config auto",
			t:    task.Task{ID: "t2", HeadlessPermissionMode: "bypass"},
			cfg:  cfgAuto,
			want: "bypass",
		},
		{
			name: "task empty falls back to config auto",
			t:    task.Task{ID: "t3"},
			cfg:  cfgAuto,
			want: "auto",
		},
		{
			name: "task empty, config empty → bypass default",
			t:    task.Task{ID: "t4"},
			cfg:  cfgEmpty,
			want: "bypass",
		},
		{
			name: "task empty, nil config → bypass default",
			t:    task.Task{ID: "t5"},
			cfg:  nil,
			want: "bypass",
		},
		{
			name:    "invalid task value → error (abort, no fallback)",
			t:       task.Task{ID: "t6", HeadlessPermissionMode: "dangerously-skip"},
			cfg:     cfgBypass,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveHeadlessPermissionMode(tc.t, tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// readAuditEvents reads all NDJSON events from the audit directory.
func readAuditEvents(t *testing.T, dir string) []audit.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read audit dir: %v", err)
	}
	var events []audit.Event
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ndjson") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("open audit file: %v", err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev audit.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("unmarshal audit event: %v", err)
			}
			events = append(events, ev)
		}
		_ = f.Close()
	}
	return events
}

// newMinimalTaskManager creates a real task.Manager backed by a temp directory.
func newMinimalTaskManager(t *testing.T) *task.Manager {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return task.NewManager(store, nil)
}

func TestOnComplete_EmitsPermissionDeniedAuditEvents(t *testing.T) {
	t.Parallel()

	auditDir := t.TempDir()
	al, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = al.Close() })

	taskMgr := newMinimalTaskManager(t)
	tk, err := taskMgr.Create("test task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	h := &AgentCompletionHandler{
		DomainHandler: DomainHandler{
			audit:  al,
			logger: slog.New(slog.DiscardHandler),
		},
		tasks: taskMgr,
	}

	// Add an AgentRun so UpdateRun doesn't fail with "agent run not found".
	if err := taskMgr.AddRun(tk.ID, task.AgentRun{
		AgentID: "agent-1",
		Role:    "implementation",
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{
		TaskID: tk.ID,
		ID:     "agent-1",
	}
	ag.NotePermissionDenial("tooluseID-1", "denied by the claude code auto mode classifier: rm -rf /")
	ag.NotePermissionDenial("", "denied by the claude code auto mode classifier: git push --force")

	h.OnComplete(ag)

	_ = al.Close()

	events := readAuditEvents(t, auditDir)
	var denials []audit.Event
	for _, ev := range events {
		if ev.Type == audit.EventAgentPermissionDenied {
			denials = append(denials, ev)
		}
	}

	if len(denials) != 2 {
		t.Fatalf("expected 2 permission_denied events, got %d (all events: %v)", len(denials), events)
	}

	d0 := denials[0]
	if d0.TaskID != tk.ID {
		t.Errorf("d0 TaskID = %q, want %q", d0.TaskID, tk.ID)
	}
	if d0.Data["tool"] != "tooluseID-1" {
		t.Errorf("d0 tool = %v, want tooluseID-1", d0.Data["tool"])
	}
	if !strings.Contains(d0.Data["reason"].(string), "denied by") {
		t.Errorf("d0 reason missing denial text: %v", d0.Data["reason"])
	}

	// Second denial: empty toolUseID → "unknown" fallback
	d1 := denials[1]
	if d1.Data["tool"] != "unknown" {
		t.Errorf("d1 tool = %v, want 'unknown'", d1.Data["tool"])
	}
}

func TestOnComplete_NoDenialsNoPermissionDeniedEvents(t *testing.T) {
	t.Parallel()

	auditDir := t.TempDir()
	al, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = al.Close() })

	taskMgr := newMinimalTaskManager(t)
	tk, err := taskMgr.Create("clean task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := taskMgr.AddRun(tk.ID, task.AgentRun{
		AgentID: "agent-clean",
		Role:    "implementation",
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatal(err)
	}

	h := &AgentCompletionHandler{
		DomainHandler: DomainHandler{
			audit:  al,
			logger: slog.New(slog.DiscardHandler),
		},
		tasks: taskMgr,
	}

	ag := &agent.Agent{TaskID: tk.ID, ID: "agent-clean"}
	h.OnComplete(ag)
	_ = al.Close()

	events := readAuditEvents(t, auditDir)
	for _, ev := range events {
		if ev.Type == audit.EventAgentPermissionDenied {
			t.Errorf("unexpected permission_denied event when no denials recorded: %+v", ev)
		}
	}
}
