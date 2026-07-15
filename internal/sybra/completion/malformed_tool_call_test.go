package completion

import (
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/task"
)

func TestOnComplete_EmitsMalformedToolCallAuditEvents(t *testing.T) {
	t.Parallel()

	auditDir := t.TempDir()
	al, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = al.Close() })

	taskMgr := newMinimalTaskManager(t)
	tk, err := taskMgr.Create("malformed tool task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := taskMgr.AddRun(tk.ID, task.AgentRun{
		AgentID: "agent-malformed",
		Role:    "implementation",
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatal(err)
	}

	h := New(Config{Audit: al, Logger: discardLogger(), Tasks: taskMgr})

	ag := &agent.Agent{TaskID: tk.ID, ID: "agent-malformed", Provider: "claude"}
	ag.NoteMalformedToolCall("toolu_1", "functions.exec_command", "corrected")
	ag.NoteMalformedToolCall("", "", "unrecoverable")

	h.OnComplete(ag)
	_ = al.Close()

	events := readAuditEvents(t, auditDir)
	var corrected, unrecoverable *audit.Event
	for i := range events {
		switch events[i].Type {
		case audit.EventAgentMalformedToolCorrected:
			corrected = &events[i]
		case audit.EventAgentMalformedToolUnrecoverable:
			unrecoverable = &events[i]
		}
	}
	if corrected == nil {
		t.Fatal("missing corrected malformed-tool audit event")
	}
	if unrecoverable == nil {
		t.Fatal("missing unrecoverable malformed-tool audit event")
	}
	if corrected.Data["provider"] != "claude" {
		t.Fatalf("corrected provider = %v, want claude", corrected.Data["provider"])
	}
	if corrected.Data["tool"] != "functions.exec_command" {
		t.Fatalf("corrected tool = %v, want functions.exec_command", corrected.Data["tool"])
	}
	if corrected.Data["tool_use_id"] != "toolu_1" {
		t.Fatalf("corrected tool_use_id = %v, want toolu_1", corrected.Data["tool_use_id"])
	}
	if unrecoverable.Data["tool"] != "unknown" {
		t.Fatalf("unrecoverable tool = %v, want unknown", unrecoverable.Data["tool"])
	}
	if unrecoverable.Data["tool_use_id"] != "unknown" {
		t.Fatalf("unrecoverable tool_use_id = %v, want unknown", unrecoverable.Data["tool_use_id"])
	}
}
