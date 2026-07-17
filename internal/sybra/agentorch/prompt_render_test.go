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
	"github.com/Automaat/sybra/internal/sybra/completion"
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

// TestRecordImplAgentStart_PromptHashCorrelatesWithCompletion proves the
// dispatch-time prompt_hash stamped by recordImplAgentStart (agent.started)
// matches the completion-time prompt_hash on agent.prompt_rendered — the join
// key diagnostics use to correlate a run's provider render summary back to
// its dispatch record, without either event ever carrying prompt text.
func TestRecordImplAgentStart_PromptHashCorrelatesWithCompletion(t *testing.T) {
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
	tk, err := tm.Create("prompt render task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	o := New(tm, nil, nil, al, discardSlogLogger(), nil, &config.Config{})

	// ag.Prompt is the prepared prompt Run dispatched (post NOTES.md/guardrail/
	// skill preparation) — recordImplAgentStart hashes this, not the
	// pre-preparation prompt passed as fullPrompt.
	ag := &agent.Agent{ID: "agent-1", TaskID: tk.ID, Provider: "codex", Prompt: "Run /plan-critic now\n\n<NOTES.md scratchpad>"}
	// Mirrors what codexProvider.BuildHeadlessInvocation stamps before
	// dispatch records the run.
	ag.SetPromptRender("slash-to-dollar", []string{"plan-critic"}, nil)

	o.recordImplAgentStart(ag, tk, tk.ID, "headless", "bypass", false, false, "Run /plan-critic now")

	h := completion.New(completion.Config{Logger: discardSlogLogger(), Audit: al, Tasks: tm})
	h.OnComplete(ag)
	_ = al.Close()

	events := readAuditNDJSON(t, auditDir)
	var started, rendered *audit.Event
	for i := range events {
		switch events[i].Type {
		case audit.EventAgentStarted:
			started = &events[i]
		case audit.EventAgentPromptRendered:
			rendered = &events[i]
		}
	}
	if started == nil {
		t.Fatal("no agent.started event recorded")
	}
	if rendered == nil {
		t.Fatal("no agent.prompt_rendered event recorded")
	}

	startedHash, _ := started.Data["prompt_hash"].(string)
	renderedHash, _ := rendered.Data["prompt_hash"].(string)
	if startedHash == "" {
		t.Fatal("agent.started prompt_hash is empty")
	}
	if startedHash != renderedHash {
		t.Fatalf("prompt_hash mismatch: started=%q rendered=%q", startedHash, renderedHash)
	}
}
