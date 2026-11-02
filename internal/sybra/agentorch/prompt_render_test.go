package agentorch

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

// readAuditNDJSON reads every audit event persisted under dir, mirroring the
// completion package's own readAuditEvents test helper.
func readAuditNDJSON(t *testing.T, dir string) []audit.Event {
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

// TestRecordImplAgentStart_HashesDispatchedPrompt proves agent.started carries
// the hash of the prepared prompt, not the pre-preparation one, without ever
// carrying prompt text.
func TestRecordImplAgentStart_HashesDispatchedPrompt(t *testing.T) {
	auditDir := t.TempDir()
	al, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = al.Close() })

	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	tk, err := tm.Create("prompt hash task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	o := New(tm, nil, nil, al, discardSlogLogger(), nil, &config.Config{})
	ag := &agent.Agent{ID: "agent-1", TaskID: tk.ID, Provider: "codex", Mode: "headless", Prompt: "Implement it\n\n<NOTES.md scratchpad>"}

	o.recordImplAgentStart(ag, tk, tk.ID, "headless", "bypass", false, false, "Implement it")
	_ = al.Close()

	for _, event := range readAuditNDJSON(t, auditDir) {
		if event.Type != audit.EventAgentStarted {
			continue
		}
		if got, want := event.Data["prompt_hash"], agent.HashPrompt(ag.Prompt); got != want {
			t.Fatalf("prompt_hash = %v, want %q", got, want)
		}
		return
	}
	t.Fatal("no agent.started event recorded")
}

func TestBuildTaskStartPrompt_AppendsAttachmentPathsOnly(t *testing.T) {
	t.Parallel()

	got := BuildTaskStartPrompt(task.Task{
		Title: "attachment prompt task",
		Body:  "Investigate the attached evidence.",
		Attachments: []task.Attachment{{
			ID:          "att-1",
			FileName:    "evidence.png",
			ContentType: "image/png",
			SizeBytes:   2048,
			Path:        "/tmp/sybra/attachments/task-1/att-1/evidence.png",
		}},
	}, "Implement the fix.", true)

	if !strings.Contains(got, "## Attachments") {
		t.Fatalf("prompt missing attachments section:\n%s", got)
	}
	if !strings.Contains(got, "evidence.png | 2048 bytes | image/png | /tmp/sybra/attachments/task-1/att-1/evidence.png") {
		t.Fatalf("prompt missing attachment metadata line:\n%s", got)
	}
	if strings.Contains(got, "iVBOR") {
		t.Fatalf("prompt appears to inline attachment bytes:\n%s", got)
	}
}
