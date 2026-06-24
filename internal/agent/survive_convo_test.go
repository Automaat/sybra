package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWillDetach(t *testing.T) {
	cases := []struct {
		mode    string
		oneShot bool
		want    bool
	}{
		{"headless", false, true},
		{"headless", true, true},
		{"interactive", false, true},
		{"interactive", true, true}, // one-shot survives too
		{"chat", false, false},      // unknown mode
		{"", false, false},          // empty mode
	}
	for _, c := range cases {
		got := willDetach(RunConfig{Mode: c.mode, OneShot: c.oneShot})
		if got != c.want {
			t.Errorf("willDetach(mode=%s oneShot=%v)=%v, want %v", c.mode, c.oneShot, got, c.want)
		}
	}
}

// TestBuildOneShotConvoArgs verifies a one-shot survival turn passes the
// prompt as an argument with NO --input-format — claude only reads
// stream-json from a pipe, not a regular file, so one-shot survival must
// feed the prompt via argv, not stdin.
func TestBuildOneShotConvoArgs(t *testing.T) {
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	a := &Agent{ID: "x", Provider: "claude", Model: "sonnet"}
	args := m.buildOneShotConvoArgs(a, RunConfig{Prompt: "do the thing", RequirePermissions: false})

	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--input-format") {
		t.Fatalf("one-shot must not use --input-format (stdin stream-json): %q", joined)
	}
	// The prompt must be the argument immediately after -p.
	found := false
	for i := range len(args) - 1 {
		if args[i] == "-p" {
			if args[i+1] != "do the thing" {
				t.Fatalf("expected prompt as arg after -p, got %q", args[i+1])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected -p <prompt> in args: %q", joined)
	}
	if !strings.Contains(joined, "--output-format stream-json") {
		t.Fatalf("expected stream-json output: %q", joined)
	}
}

func TestRehydrateCodexConvoFromLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.ndjson")
	// Minimal codex stream-json: a session init then an agent message.
	lines := `{"type":"thread.started","thread_id":"cx-1"}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &Agent{ID: "cx", Provider: "codex"}
	rehydrateCodexConvoFromLog(a, path)
	if len(a.ConvoOutput()) == 0 {
		t.Fatal("expected codex convo events rehydrated")
	}
}

// TestReattachCodexConvo_RecreatesIdleAgent verifies a codex interactive
// record is recreated as an idle, sendable agent on restart (no live process
// required — codex has none between turns).
func TestReattachCodexConvo_RecreatesIdleAgent(t *testing.T) {
	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "cx9.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := `{"type":"thread.started","thread_id":"cx-9"}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}` + "\n"
	if err := os.WriteFile(logPath, []byte(lines), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	if err := m.EnableSurviveRestart(regDir); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	// Codex record: PID 0 (no live process between turns).
	rec := Record{ID: "cx9", TaskID: "t-cx", Mode: "interactive", Provider: "codex", PID: 0, LogPath: logPath, SessionID: "cx-9", StartedAt: time.Now().UTC()}
	if err := m.reg.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := m.ReattachAll()
	if len(got) != 1 {
		t.Fatalf("expected 1 recreated codex agent, got %d", len(got))
	}
	a, err := m.GetAgent("cx9")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.GetState() != StatePaused {
		t.Fatalf("expected recreated codex agent Paused, got %s", a.GetState())
	}
	if len(a.ConvoOutput()) == 0 {
		t.Fatal("expected rehydrated codex convo buffer")
	}
	// The recreated agent is sendable: it has a live prompt channel. (Don't
	// actually send — that would spawn a real codex process.)
	a.mu.RLock()
	hasPromptCh := a.promptCh != nil
	a.mu.RUnlock()
	if !hasPromptCh {
		t.Fatal("recreated codex agent should have a prompt channel")
	}

	// Stop and wait for the goroutine to exit before TempDir cleanup.
	if err := m.StopAgent("cx9"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	select {
	case <-a.done:
	case <-time.After(3 * time.Second):
		t.Fatal("recreated codex agent did not exit after StopAgent")
	}

	// History preservation: recreate must reuse the existing log (append),
	// not open a new empty file — otherwise the next restart loses history.
	if a.GetLogPath() != logPath {
		t.Fatalf("recreate must reuse the existing log, got %q want %q", a.GetLogPath(), logPath)
	}
}

// TestReattachCodexConvo_SkipsDeletedTask verifies a codex record whose task
// no longer exists is dropped (not recreated as a zombie agent).
func TestReattachCodexConvo_SkipsDeletedTask(t *testing.T) {
	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "gone1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(`{"type":"thread.started","thread_id":"g"}`+"\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	if err := m.EnableSurviveRestart(regDir); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	m.SetTaskExists(func(string) bool { return false }) // task is gone

	if err := m.reg.Save(Record{ID: "gone1", TaskID: "deleted", Mode: "interactive", Provider: "codex", LogPath: logPath, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := m.ReattachAll(); len(got) != 0 {
		t.Fatalf("expected no recreate for deleted task, got %d", len(got))
	}
	if _, err := m.GetAgent("gone1"); err == nil {
		t.Fatal("expected no agent registered for deleted task")
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatal("expected record deleted for gone task")
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

// TestProcessConvoLine_RegistryGatedByDetached locks in the fix for the
// one-shot leak: only a detached agent persists a registry record; a
// non-detached (one-shot/legacy) interactive agent must not, while still
// capturing the session id.
func TestProcessConvoLine_RegistryGatedByDetached(t *testing.T) {
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	if err := m.EnableSurviveRestart(t.TempDir()); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	st := &convoEmitState{}

	// Non-detached one-shot: session captured, NO registry record.
	a := &Agent{ID: "os1", Mode: "interactive", Provider: "claude"}
	m.processConvoLine(a, []byte(`{"type":"system","subtype":"init","session_id":"s1"}`), st, true)
	if a.GetSessionID() != "s1" {
		t.Fatalf("expected session captured even when not detached, got %q", a.GetSessionID())
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatalf("non-detached agent must not write a registry record, got %d", len(list))
	}

	// Detached survival agent: record written.
	a2 := &Agent{ID: "d1", Mode: "interactive", Provider: "claude", StartedAt: time.Now().UTC()}
	a2.setDetached(true)
	m.processConvoLine(a2, []byte(`{"type":"system","subtype":"init","session_id":"s2"}`), st, false)
	if list, _ := m.reg.List(); len(list) != 1 {
		t.Fatalf("detached agent must write a registry record, got %d", len(list))
	}
}

func TestConvoResumeState(t *testing.T) {
	cases := []struct {
		name   string
		events []ConvoEvent
		want   State
	}{
		{"empty", nil, StatePaused},
		{"ended with result", []ConvoEvent{{Type: "assistant"}, {Type: "result"}}, StatePaused},
		{"mid-generation after result", []ConvoEvent{{Type: "result"}, {Type: "assistant"}}, StateRunning},
		{"assistant only", []ConvoEvent{{Type: "system"}, {Type: "assistant"}}, StateRunning},
	}
	for _, c := range cases {
		if got := convoResumeState(c.events); got != c.want {
			t.Errorf("%s: convoResumeState=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestRegistryDelete_RemovesFIFO(t *testing.T) {
	s, err := newRegistryStore(t.TempDir())
	if err != nil {
		t.Fatalf("newRegistryStore: %v", err)
	}
	if err := s.Save(Record{ID: "f1", Mode: "interactive", Provider: "claude"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	fifo := s.fifoPath("f1")
	if err := makeFIFO(fifo); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if err := s.Delete("f1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(fifo); !os.IsNotExist(err) {
		t.Fatalf("expected FIFO removed on Delete, stat err=%v", err)
	}
}

// TestReattachInteractive_OneShotTailOnly verifies a one-shot survival
// record (no FIFO / empty StdinPath) reattaches tail-only and finalizes
// when its single turn completes — no FIFO reopen required.
func TestReattachInteractive_OneShotTailOnly(t *testing.T) {
	prev := reattachPIDPoll
	reattachPIDPoll = 50 * time.Millisecond
	t.Cleanup(func() { reattachPIDPoll = prev })

	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "os9.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	history := `{"type":"system","subtype":"init","session_id":"sess-os"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(history), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	if err := m.EnableSurviveRestart(regDir); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	var completed atomic.Bool
	m.SetOnComplete(func(*Agent) { completed.Store(true) })

	result := `{"type":"result","result":"done","session_id":"sess-os","total_cost_usd":0.1}`
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep 0.3; printf '%%s\\n' '%s' >> %q", result, logPath))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	go func() { _ = cmd.Wait() }()

	// One-shot survival record: StdinPath intentionally empty.
	rec := Record{
		ID: "os9", TaskID: "t-os", Mode: "interactive", Provider: "claude",
		PID: cmd.Process.Pid, LogPath: logPath, StdinPath: "",
		SessionID: "sess-os", StartedAt: time.Now().UTC(), ProcStartedAt: processStartString(cmd.Process.Pid),
	}
	if err := m.reg.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := m.ReattachAll()
	if len(got) != 1 {
		t.Fatalf("expected 1 reattached one-shot agent, got %d", len(got))
	}
	if _, err := m.GetAgent("os9"); err != nil {
		t.Fatalf("GetAgent: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for !completed.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for one-shot reattach to complete")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatalf("expected registry empty after completion, got %d", len(list))
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
