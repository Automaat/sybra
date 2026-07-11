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
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
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
	rehydratePerTurnConvoFromLog(a, path)
	if len(a.ConvoOutput()) == 0 {
		t.Fatal("expected codex convo events rehydrated")
	}
}

func TestProcessConvoLine_IgnoresProviderMarker(t *testing.T) {
	m, _ := newTestManager(t)
	a := &Agent{ID: "marker", Provider: "claude"}
	st := &convoEmitState{}

	m.processConvoLine(context.Background(), a, []byte(`{"__sybra_provider_marker__":"provider_switch","version":1,"provider":"copilot"}`), st, false)

	if got := len(a.ConvoOutput()); got != 0 {
		t.Fatalf("expected provider marker to be ignored, got %d convo events", got)
	}
}

func TestReopenConvoHandoffLog_FallsBackToFreshFile(t *testing.T) {
	logDir := t.TempDir()
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	a := &Agent{ID: "handoff-fallback"}
	badPath := filepath.Join(t.TempDir(), "stale-log")
	if err := os.MkdirAll(badPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a.SetLogPath(badPath)

	f, err := m.reopenConvoHandoffLog(a, nil)
	if err != nil {
		t.Fatalf("reopenConvoHandoffLog: %v", err)
	}
	defer func() { _ = f.Close() }()

	if f.Name() == badPath {
		t.Fatalf("expected fallback fresh log, kept broken path %q", badPath)
	}
	if a.GetLogPath() != f.Name() {
		t.Fatalf("expected agent log path updated to fallback file, got %q want %q", a.GetLogPath(), f.Name())
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

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{SurviveRestartDir: regDir})
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
	if !a.hasPromptChannel() {
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

func TestSaveRegistry_PreservesOneShot(t *testing.T) {
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{SurviveRestartDir: t.TempDir()})
	m.saveRegistry(context.Background(), &Agent{
		ID:        "cx-one",
		TaskID:    "t-cx",
		Mode:      "interactive",
		Provider:  "codex",
		StartedAt: time.Now().UTC(),
		oneShot:   true,
	})

	recs, err := m.reg.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if !recs[0].OneShot {
		t.Fatal("expected one-shot lifecycle bit persisted")
	}
}

// TestReattachCodexConvo_OneShotNoResultDropped verifies a workflow-owned
// per-turn Codex run interrupted before a terminal result is dropped without
// firing a completion callback — finalizing it as failed would burn the
// workflow step's retry budget on a turn that never produced a result, and
// resurrecting it as an idle chat would hide it from
// RestartStaleInProgress's interactive-oneshot redispatch.
func TestReattachCodexConvo_OneShotNoResultDropped(t *testing.T) {
	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "cx1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := `{"type":"thread.started","thread_id":"cx-1"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"working"}}` + "\n"
	if err := os.WriteFile(logPath, []byte(lines), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var completed atomic.Bool
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete: func(a *Agent) {
			completed.Store(true)
		},
	})

	rec := Record{
		ID: "cx1", TaskID: "t-cx", Mode: "interactive", Provider: "codex",
		PID: 0, LogPath: logPath, SessionID: "cx-1", StartedAt: time.Now().UTC(), OneShot: true,
	}
	if err := m.reg.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := m.ReattachAll(); len(got) != 0 {
		t.Fatalf("expected interrupted one-shot to be dropped, not recreated, got %d agents", len(got))
	}
	if completed.Load() {
		t.Fatal("expected no completion callback for a turn that never produced a result — it must not burn the workflow step's retry budget")
	}
	if _, err := m.GetAgent("cx1"); err == nil {
		t.Fatal("expected no idle agent registered for interrupted one-shot")
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatalf("expected registry empty after dropping the interrupted one-shot, got %d", len(list))
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

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		TaskExists:        func(string) bool { return false }, // task is gone
	})

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
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{SurviveRestartDir: t.TempDir()})
	st := &convoEmitState{}

	// Non-detached one-shot: session captured, NO registry record.
	a := &Agent{ID: "os1", Mode: "interactive", Provider: "claude"}
	m.processConvoLine(context.Background(), a, []byte(`{"type":"system","subtype":"init","session_id":"s1"}`), st, true)
	if a.GetSessionID() != "s1" {
		t.Fatalf("expected session captured even when not detached, got %q", a.GetSessionID())
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatalf("non-detached agent must not write a registry record, got %d", len(list))
	}

	// Detached survival agent: record written.
	a2 := &Agent{ID: "d1", Mode: "interactive", Provider: "claude", StartedAt: time.Now().UTC()}
	a2.setDetached(true)
	m.processConvoLine(context.Background(), a2, []byte(`{"type":"system","subtype":"init","session_id":"s2"}`), st, false)
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
	regDir := t.TempDir()
	s, err := newRegistryStore(regDir)
	if err != nil {
		t.Fatalf("newRegistryStore: %v", err)
	}
	fifo := agentFIFOPath(regDir, "f1")
	if err := s.Save(Record{ID: "f1", Mode: "interactive", Provider: "claude", StdinPath: fifo}); err != nil {
		t.Fatalf("save: %v", err)
	}
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
	prev := reattachPIDPoll.Load()
	reattachPIDPoll.Store(int64(50 * time.Millisecond))
	t.Cleanup(func() { reattachPIDPoll.Store(prev) })

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

	var completed atomic.Bool
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete:        func(*Agent) { completed.Store(true) },
	})

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
		SessionID: "sess-os", StartedAt: time.Now().UTC(), ProcStartedAt: processStartString(context.Background(), cmd.Process.Pid),
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
	prev := reattachPIDPoll.Load()
	reattachPIDPoll.Store(int64(50 * time.Millisecond))
	t.Cleanup(func() { reattachPIDPoll.Store(prev) })

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

	var completed atomic.Bool
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete:        func(*Agent) { completed.Store(true) },
	})

	// FIFO must exist for the reopen to succeed.
	fifoPath := agentFIFOPath(regDir, "ic1")
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
		SessionID: "sess-ic", StartedAt: time.Now().UTC(), ProcStartedAt: processStartString(context.Background(), cmd.Process.Pid),
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

// TestReattachConvo_SameProcessHandoffSkipsOrdinaryFinalization verifies
// that when the *Agent object being reattached still carries an in-memory
// pending handoff (SendMessage/advanceClaudeTurn regated it onto a peer
// right before its now-doomed Claude process was torn down), reattachConvo
// hands off to the peer via completeConvoHandoff instead of finalizing the
// agent as stopped. This only happens for the same in-memory Agent — a
// genuine restart's fromRecord Agent never carries a handoff (see
// TestReattachInteractive_ReattachesLiveAgent for that path).
func TestReattachConvo_SameProcessHandoffSkipsOrdinaryFinalization(t *testing.T) {
	dir := t.TempDir()
	writeFakePerTurnBinary(t, dir, "copilot",
		`{"type":"assistant.message","data":{"content":"hi from copilot"}}`,
		`{"type":"result","sessionId":"cop-fake"}`,
	)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "hoff1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	history := `{"__sybra_provider_marker__":"provider_switch","version":1,"provider":"claude"}` + "\n" +
		`{"type":"system","session_id":"claude-sess"}` + "\n" +
		`{"type":"result","subtype":"success","session_id":"claude-sess"}` + "\n"
	if err := os.WriteFile(logPath, []byte(history), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var completed atomic.Bool
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete:        func(*Agent) { completed.Store(true) },
	})
	m.SetHealthGate(&fakeGate{healthy: map[string]bool{"copilot": true}})

	// A short-lived helper process stands in for the doomed Claude process:
	// it exits almost immediately, which is what should make reattachConvo's
	// tail loop return exited=true.
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	procStart := processStartString(context.Background(), cmd.Process.Pid)
	go func() { _ = cmd.Wait() }()

	a := &Agent{ID: "hoff1", TaskID: "t-hoff1", Provider: "claude", Mode: "interactive", done: make(chan struct{})}
	a.SetLogPath(logPath)
	// Mirrors real usage: regateBeforeClaudeTurn (called from advanceClaudeTurn
	// before beginConvoHandoff/SetPendingHandoff) already flips a.Provider to
	// the peer before the handoff is recorded.
	a.SetProviderAndModel("copilot", "")
	a.SetPendingHandoff(RunConfig{TaskID: "t-hoff1", Provider: "copilot"}, "continue-on-peer")

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	t.Cleanup(cancel)
	m.mu.Lock()
	m.agents[a.ID] = a
	m.liveCount = 1
	m.liveByProvider["claude"] = 1
	m.mu.Unlock()

	m.reattachConvo(ctx, a, 0, procStart, false)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && a.GetSessionID() != "cop-fake" {
		time.Sleep(5 * time.Millisecond)
	}
	if a.GetSessionID() != "cop-fake" {
		t.Fatalf("expected copilot session id after handoff, got %q", a.GetSessionID())
	}
	waitForAgentState(t, a, StatePaused, testStateWaitTimeout)

	var sawHandoffPrompt bool
	for _, ev := range a.ConvoOutput() {
		if ev.Type == "result" && ev.SessionID == "cop-fake" {
			sawHandoffPrompt = true
		}
	}
	if !sawHandoffPrompt {
		t.Error("expected the handed-off prompt to run on copilot")
	}
	// completeConvoHandoff hands the agent to a fresh runPerTurnConversational
	// goroutine rather than finalizing it, so OnComplete must not have fired
	// yet — the ordinary reattachConvo finalization path was skipped.
	if completed.Load() {
		t.Error("expected ordinary finalization to be skipped by the handoff, but OnComplete already fired")
	}

	if err := m.StopAgent(a.ID); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	select {
	case <-a.done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not exit after StopAgent")
	}
}

// TestSurviveHeadlessStoresAndReopensFIFO verifies runHeadlessAttemptSurvive's
// FIFO wiring for a steerable headless claude run: the FIFO path is persisted
// on the registry record (mirroring startConvoProcessSurvive), and a message
// written through it actually reaches the detached child.
func TestSurviveHeadlessStoresAndReopensFIFO(t *testing.T) {
	binDir := makeFakeEchoStdinClaude(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logDir := t.TempDir()
	regDir := t.TempDir()
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{SurviveRestartDir: regDir})

	a := &Agent{ID: "sv1", TaskID: "t-sv1", Mode: "headless", Provider: "claude", done: make(chan struct{})}
	cfg := RunConfig{Prompt: "first turn", HeadlessSteerable: true}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}

	var outFile *os.File
	var tailOffset int64
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resultCh := make(chan error, 1)
	go func() {
		_, err := m.runHeadlessAttemptSurvive(ctx, a, cfg, &outFile, &tailOffset, "claude", args, nil, "claude")
		resultCh <- err
	}()

	// Wait for the FIFO to be assigned and persisted.
	deadline := time.After(5 * time.Second)
	for a.GetStdinPath() == "" {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for stdin FIFO to be assigned")
		case <-time.After(20 * time.Millisecond):
		}
	}

	wantFIFO := agentFIFOPath(regDir, "sv1")
	if a.GetStdinPath() != wantFIFO {
		t.Fatalf("GetStdinPath() = %q, want %q", a.GetStdinPath(), wantFIFO)
	}

	var recs []Record
	deadline = time.After(5 * time.Second)
	for {
		var err error
		recs, err = m.reg.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(recs) == 1 && recs[0].StdinPath == wantFIFO {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for registry record with StdinPath; got %+v", recs)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// The initial prompt was delivered over the FIFO — wait for the fake
	// binary's echoed result to land in the log.
	logPath := a.GetLogPath()
	deadline = time.After(10 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "first turn") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for initial prompt to echo in log; got %q", string(data))
		case <-time.After(50 * time.Millisecond):
		}
	}

	// End the test quickly rather than waiting on the never-EOF FIFO or the
	// 90s post-result-hang grace window.
	m.signalKill(a)
	select {
	case <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("runHeadlessAttemptSurvive did not return after signalKill")
	}
	// Unblocks stopWithSIGINT's internal SIGKILL-escalation goroutine
	// immediately instead of leaving it sleeping out stopSIGINTGrace —
	// signalKill's real callers close this via markAgentDone once the
	// runner goroutine fully exits.
	close(a.done)
}

// TestReattachHeadlessReopensFIFO verifies that reattaching a live steerable
// headless survive record reopens its stdin FIFO (mirroring
// TestReattachInteractive_ReattachesLiveAgent for the conversational path),
// so a steer message sent after reattach still reaches the child.
func TestReattachHeadlessReopensFIFO(t *testing.T) {
	prev := reattachPIDPoll.Load()
	reattachPIDPoll.Store(int64(50 * time.Millisecond))
	t.Cleanup(func() { reattachPIDPoll.Store(prev) })

	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "hd1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	history := `{"type":"system","subtype":"init","session_id":"sess-hd"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"earlier"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(history), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var completed atomic.Bool
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete:        func(*Agent) { completed.Store(true) },
	})

	fifoPath := agentFIFOPath(regDir, "hd1")
	if err := makeFIFO(fifoPath); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	result := `{"type":"result","result":"ok","session_id":"sess-hd","total_cost_usd":0.1}`
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep 0.3; printf '%%s\\n' '%s' >> %q", result, logPath))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	go func() { _ = cmd.Wait() }()

	rec := Record{
		ID: "hd1", TaskID: "t-hd", Mode: "headless", Provider: "claude",
		PID: cmd.Process.Pid, LogPath: logPath, StdinPath: fifoPath,
		SessionID: "sess-hd", StartedAt: time.Now().UTC(), ProcStartedAt: processStartString(context.Background(), cmd.Process.Pid),
	}
	if err := m.reg.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := m.ReattachAll()
	if len(got) != 1 {
		t.Fatalf("expected 1 reattached headless agent, got %d", len(got))
	}
	a, err := m.GetAgent("hd1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.GetStdinPath() != fifoPath {
		t.Fatalf("expected FIFO path restored, got %q", a.GetStdinPath())
	}

	// reattachHeadless reopens the FIFO from its own goroutine
	// (go m.reattachHeadless(...) in ReattachAllContext), so poll rather than
	// assert immediately after ReattachAll returns.
	fifoDeadline := time.After(5 * time.Second)
	for !a.convo.hasStdinPipe() {
		select {
		case <-fifoDeadline:
			t.Fatal("expected FIFO reopened as the agent's stdin transport")
		case <-time.After(20 * time.Millisecond):
		}
	}

	deadline := time.After(5 * time.Second)
	for !completed.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for reattached headless agent to complete")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if a.GetState() != StateStopped {
		t.Fatalf("expected stopped after completion, got %s", a.GetState())
	}
}
