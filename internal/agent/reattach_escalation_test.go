package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFromRecordHeadlessRestoresEscalationChannel(t *testing.T) {
	a := fromRecord(Record{ID: "a", Mode: "headless"})
	if a.escalationCh == nil {
		t.Fatal("headless reattach omitted escalation channel")
	}
	if err := (&Manager{}).RespondEscalation("missing", true); err == nil {
		t.Fatal("sanity: manager should reject missing agent")
	}
}

func TestTailHeadlessFile_ShutdownDuringTurnsEscalationDetaches(t *testing.T) {
	escalated := make(chan struct{}, 1)
	m := mustNewManager(t, context.Background(), func(event string, _ any) {
		if strings.HasPrefix(event, "agent:escalation:") {
			escalated <- struct{}{}
		}
	}, slog.New(slog.DiscardHandler), t.TempDir())
	m.SetGuardrails(Guardrails{MaxTurns: 1})
	logPath := filepath.Join(t.TempDir(), "run.ndjson")
	if err := os.WriteFile(logPath, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"tick"}]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{ID: "a", Name: RoleReview.AgentName("task"), Provider: "claude", escalationCh: make(chan bool, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	procDone := make(chan struct{})
	done := make(chan bool, 1)
	go func() { exited, _ := m.tailHeadlessFile(ctx, a, logPath, 0, procDone); done <- exited }()
	select {
	case <-escalated:
	case <-time.After(time.Second):
		t.Fatal("turns escalation was not raised")
	}
	cancel()
	select {
	case exited := <-done:
		if exited {
			t.Fatal("shutdown during pending escalation finalized instead of detaching")
		}
	case <-time.After(time.Second):
		t.Fatal("tailer did not exit after shutdown")
	}
}
