package agent

import (
	"context"
	"io"
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

func TestResultBeforeOnlyForkOutput(t *testing.T) {
	events := []StreamEvent{{Type: "result"}, {Type: "assistant", parentToolUseID: "fork"}, {Type: "result", Subtype: "error", parentToolUseID: "fork"}, {Type: "system", Subtype: "background_tasks_changed"}}
	if found, isError := resultBeforeOnlyForkOutput(events); !found || isError {
		t.Fatalf("resultBeforeOnlyForkOutput = (%v, %v), want clean result", found, isError)
	}
	if found, _ := resultBeforeOnlyForkOutput([]StreamEvent{{Type: "result"}, {Type: "init"}}); found {
		t.Fatal("top-level retry event must hide prior result")
	}
}

func TestReattachErrorResultPreservesQueuedSteer(t *testing.T) {
	m, _ := newTestManager(t)
	a := &Agent{ID: "reattach-error-steer", TaskID: "task-1", Mode: "headless", Provider: "claude"}
	r, w := io.Pipe()
	if err := a.convo.installStdinPipe(w); err != nil {
		t.Fatalf("installStdinPipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	a.EnqueuePrompt("preserve across retry")
	a.AppendOutput(StreamEvent{Type: "result", Subtype: "error", ErrorType: "overloaded_error"})

	m.reconcileReattachedHeadlessTerminalResult(a)
	if got := a.PendingPromptCount(); got != 1 {
		t.Fatalf("PendingPromptCount = %d, want preserved queued steer", got)
	}
	if !a.isFinalizing() {
		t.Fatal("reattached error result must reject new messages")
	}
	if a.convo.hasStdinPipe() {
		t.Fatal("reattached error result must close the stale stdin")
	}
}
