package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWillDetach(t *testing.T) {
	cases := []struct {
		mode     string
		provider string
		oneShot  bool
		want     bool
	}{
		{"headless", "claude", false, true},
		{"headless", "codex", false, true},
		{"interactive", "claude", false, true},
		{"interactive", "claude", true, false}, // one-shot stays on pipe path
		{"interactive", "codex", false, false}, // codex spawns per turn
		{"interactive", "", false, true},       // empty provider normalizes to claude
		{"chat", "claude", false, false},       // unknown mode
	}
	for _, c := range cases {
		got := willDetach(RunConfig{Mode: c.mode, OneShot: c.oneShot}, c.provider)
		if got != c.want {
			t.Errorf("willDetach(mode=%s provider=%s oneShot=%v)=%v, want %v", c.mode, c.provider, c.oneShot, got, c.want)
		}
	}
}

func TestMakeFIFO_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.stdin")
	if err := makeFIFO(path); err != nil {
		t.Fatalf("makeFIFO: %v", err)
	}
	// Recreate is idempotent (removes stale first).
	if err := makeFIFO(path); err != nil {
		t.Fatalf("makeFIFO recreate: %v", err)
	}

	// O_RDWR never blocks and keeps a writer present.
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open rdwr: %v", err)
	}
	defer func() { _ = w.Close() }()
	r, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open rdonly: %v", err)
	}
	defer func() { _ = r.Close() }()

	if _, err := w.WriteString("hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello\n" {
		t.Fatalf("got %q, want %q", string(buf), "hello\n")
	}
}

func TestRehydrateConvoFromLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "convo.ndjson")
	complete := `{"type":"system","subtype":"init","session_id":"sess-c"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi there"}]}}` + "\n"
	partial := `{"type":"assistant"` // no newline
	if err := os.WriteFile(path, []byte(complete+partial), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &Agent{ID: "c", Provider: "claude"}
	off := rehydrateConvoFromLog(a, path)

	if off != int64(len(complete)) {
		t.Fatalf("offset=%d, want %d (after last complete line)", off, len(complete))
	}
	if a.GetSessionID() != "sess-c" {
		t.Fatalf("expected session captured, got %q", a.GetSessionID())
	}
	if got := a.ConvoOutput(); len(got) != 2 {
		t.Fatalf("expected 2 rehydrated convo events, got %d", len(got))
	}
}

// TestReattachInteractive_ReattachesLiveAgent reattaches a live detached
// conversational agent, rehydrates its buffer, reopens its FIFO, streams a
// late event, and finalizes when the process exits.
func TestReattachInteractive_ReattachesLiveAgent(t *testing.T) {
	prev := reattachPIDPoll
	reattachPIDPoll = 50 * time.Millisecond
	t.Cleanup(func() { reattachPIDPoll = prev })

	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "ic1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	history := `{"type":"system","subtype":"init","session_id":"sess-ic"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"earlier"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(history), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	if err := m.EnableSurviveRestart(regDir); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	var completed atomic.Bool
	m.SetOnComplete(func(*Agent) { completed.Store(true) })

	// FIFO must exist for the reopen to succeed.
	fifoPath := m.reg.fifoPath("ic1")
	if err := makeFIFO(fifoPath); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	// Live helper: append a result line, then exit.
	result := `{"type":"result","result":"ok","session_id":"sess-ic","total_cost_usd":0.1}`
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep 0.3; printf '%%s\\n' '%s' >> %q", result, logPath))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	go func() { _ = cmd.Wait() }()

	rec := Record{
		ID: "ic1", TaskID: "t-ic", Mode: "interactive", Provider: "claude",
		PID: cmd.Process.Pid, LogPath: logPath, StdinPath: fifoPath,
		SessionID: "sess-ic", StartedAt: time.Now().UTC(), ProcStartedAt: processStartString(cmd.Process.Pid),
	}
	if err := m.reg.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := m.ReattachAll()
	if len(got) != 1 {
		t.Fatalf("expected 1 reattached interactive agent, got %d", len(got))
	}
	a, err := m.GetAgent("ic1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if len(a.ConvoOutput()) < 2 {
		t.Fatalf("expected rehydrated convo buffer, got %d events", len(a.ConvoOutput()))
	}
	if a.GetStdinPath() != fifoPath {
		t.Fatalf("expected FIFO path restored, got %q", a.GetStdinPath())
	}

	deadline := time.After(5 * time.Second)
	for !completed.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for reattached interactive agent to complete")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if a.GetState() != StateStopped {
		t.Fatalf("expected stopped after completion, got %s", a.GetState())
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatalf("expected registry empty after completion, got %d", len(list))
	}
}
