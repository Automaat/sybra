package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := newRegistryStore(dir)
	if err != nil {
		t.Fatalf("newRegistryStore: %v", err)
	}

	rec := Record{
		ID:        "a1",
		TaskID:    "t1",
		Mode:      "headless",
		Provider:  "claude",
		PID:       4321,
		SessionID: "sess-1",
		LogPath:   "/tmp/a1.ndjson",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.Save(rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 record, got %d", len(list))
	}
	got := list[0]
	if got.ID != rec.ID || got.PID != rec.PID || got.SessionID != rec.SessionID || got.LogPath != rec.LogPath {
		t.Fatalf("record round-trip mismatch: %+v", got)
	}

	if err := s.Delete("a1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, _ = s.List()
	if len(list) != 0 {
		t.Fatalf("expected 0 records after delete, got %d", len(list))
	}
	// Deleting a missing record is not an error.
	if err := s.Delete("a1"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestRegistryStore_RejectsEmptyID(t *testing.T) {
	s, err := newRegistryStore(t.TempDir())
	if err != nil {
		t.Fatalf("newRegistryStore: %v", err)
	}
	if err := s.Save(Record{}); err == nil {
		t.Fatal("expected error saving record with empty ID")
	}
}

func TestConfigureDetached_SetsSid(t *testing.T) {
	cmd := exec.Command("true")
	configureDetached(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("expected configureDetached to set Setsid")
	}
}

func TestReattachAlive_PIDReuseGuard(t *testing.T) {
	self := os.Getpid()
	start := processStartString(self)
	if start == "" {
		t.Skip("processStartString unavailable on this platform")
	}

	if !reattachAlive(Record{PID: self, ProcStartedAt: start}) {
		t.Fatal("expected own live process with matching start time to be alive")
	}
	if reattachAlive(Record{PID: self, ProcStartedAt: "Wed Jan  1 00:00:00 2020"}) {
		t.Fatal("expected PID-reuse guard to reject mismatched start time")
	}
	// Missing start time disables the guard (best-effort).
	if !reattachAlive(Record{PID: self}) {
		t.Fatal("expected alive process with no recorded start time to pass")
	}
	if reattachAlive(Record{PID: 0}) {
		t.Fatal("expected PID 0 to be dead")
	}
}

// TestReattachAll_ReattachesLiveHeadlessAgent spawns a live process that
// appends a terminal result to a pre-seeded log file, registers it, and
// asserts ReattachAll rebuilds the agent, rehydrates its buffer, streams
// the new result, and finalizes via onComplete when the process exits.
func TestReattachAll_ReattachesLiveHeadlessAgent(t *testing.T) {
	prev := reattachPIDPoll
	reattachPIDPoll = 50 * time.Millisecond
	t.Cleanup(func() { reattachPIDPoll = prev })

	logDir := t.TempDir()
	regDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "reat1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pre-seed history so rehydration has something to load.
	history := `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(history), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	if err := m.EnableSurviveRestart(regDir); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	var completed atomic.Bool
	var completedID atomic.Value
	m.SetOnComplete(func(ag *Agent) {
		completedID.Store(ag.ID)
		completed.Store(true)
	})

	// Live helper: after a short delay append a terminal result, then exit.
	resultLine := `{"type":"result","result":"done","session_id":"sess-1","total_cost_usd":0.1}`
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep 0.3; printf '%%s\\n' '%s' >> %q", resultLine, logPath))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	// Reap on exit so processAlive flips to false (mirrors init reaping a
	// reparented orphan in production).
	go func() { _ = cmd.Wait() }()

	rec := Record{
		ID:            "reat1",
		TaskID:        "task-1",
		Mode:          "headless",
		Provider:      "claude",
		PID:           cmd.Process.Pid,
		LogPath:       logPath,
		StartedAt:     time.Now().UTC(),
		ProcStartedAt: processStartString(cmd.Process.Pid),
	}
	if err := m.reg.Save(rec); err != nil {
		t.Fatalf("save record: %v", err)
	}

	reattached := m.ReattachAll()
	if len(reattached) != 1 {
		t.Fatalf("expected 1 reattached agent, got %d", len(reattached))
	}

	a, err := m.GetAgent("reat1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.GetState() != StateRunning {
		t.Fatalf("expected reattached agent running, got %s", a.GetState())
	}
	if got := a.Output(); len(got) == 0 {
		t.Fatal("expected rehydrated output buffer, got empty")
	}

	// Wait for the helper to exit and completion to fire.
	deadline := time.After(5 * time.Second)
	for !completed.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for reattached agent to complete")
		case <-time.After(20 * time.Millisecond):
		}
	}

	if id, _ := completedID.Load().(string); id != "reat1" {
		t.Fatalf("expected completion for reat1, got %q", id)
	}
	if a.GetState() != StateStopped {
		t.Fatalf("expected stopped after completion, got %s", a.GetState())
	}
	if a.GetExitErr() != nil {
		t.Fatalf("expected clean completion (terminal result seen), got err: %v", a.GetExitErr())
	}
	if !a.hasTerminalResult() {
		t.Fatal("expected terminal result in buffer")
	}
	// Registry record is removed on completion.
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatalf("expected registry empty after completion, got %d", len(list))
	}
}

// TestReattachAll_DropsDeadRecords verifies a record whose process is gone
// is deleted and not reattached.
func TestReattachAll_DropsDeadRecords(t *testing.T) {
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	if err := m.EnableSurviveRestart(t.TempDir()); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	// PID 0 is never alive.
	if err := m.reg.Save(Record{ID: "dead1", Mode: "headless", Provider: "claude", PID: 0}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := m.ReattachAll(); len(got) != 0 {
		t.Fatalf("expected no reattach for dead record, got %d", len(got))
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatal("expected dead record to be deleted")
	}
}

// TestTailHeadlessFile_StopVsShutdown verifies the core of the blocking
// review fix: a cancelled context finalizes (exited=true) when the agent
// was intentionally stopped, but survives (exited=false) on a plain app
// shutdown.
func TestTailHeadlessFile_StopVsShutdown(t *testing.T) {
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	logPath := filepath.Join(t.TempDir(), "tail.ndjson")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Shutdown (not stopped): ctx cancelled, process still alive -> survive.
	t.Run("shutdown survives", func(t *testing.T) {
		a := &Agent{ID: "s", Provider: "claude"}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		procDone := make(chan struct{}) // never closed: process alive
		exited, _ := m.tailHeadlessFile(ctx, a, logPath, 0, procDone)
		if exited {
			t.Fatal("expected survive (exited=false) on plain shutdown")
		}
	})

	// Intentional stop: ctx cancelled, WasStopped -> finalize once the
	// process exits.
	t.Run("stop finalizes", func(t *testing.T) {
		a := &Agent{ID: "s2", Provider: "claude"}
		a.MarkStopped()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		procDone := make(chan struct{})
		go func() {
			time.Sleep(30 * time.Millisecond)
			close(procDone) // process exits shortly after the stop signal
		}()
		exited, _ := m.tailHeadlessFile(ctx, a, logPath, 0, procDone)
		if !exited {
			t.Fatal("expected finalize (exited=true) on intentional stop")
		}
	})
}

// TestTailHeadlessFile_EndOffsetExcludesPartialLine verifies the tailer
// returns the byte offset after the last COMPLETE line, not raw bytes read
// — so a resume never skips the start of a still-incomplete trailing line.
func TestTailHeadlessFile_EndOffsetExcludesPartialLine(t *testing.T) {
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	logPath := filepath.Join(t.TempDir(), "tail.ndjson")
	complete := `{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}` + "\n"
	partial := `{"type":"result","result":"in-flight` // no newline yet
	if err := os.WriteFile(logPath, []byte(complete+partial), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &Agent{ID: "o", Provider: "claude"}
	procDone := make(chan struct{})
	close(procDone) // process already exited

	exited, end := m.tailHeadlessFile(context.Background(), a, logPath, 0, procDone)
	if !exited {
		t.Fatal("expected exited=true")
	}
	if end != int64(len(complete)) {
		t.Fatalf("expected endOffset=%d (after complete line only), got %d", len(complete), end)
	}
}

// TestReattachAll_RecoversCompletedDuringDowntime verifies a run that
// finished while the app was down (process gone, log has a terminal result)
// is finalized via onComplete instead of being dropped and re-run.
func TestReattachAll_RecoversCompletedDuringDowntime(t *testing.T) {
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "comp1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n" +
		`{"type":"result","result":"done","session_id":"sess-9","total_cost_usd":0.2}` + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	if err := m.EnableSurviveRestart(t.TempDir()); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	var completedID atomic.Value
	m.SetOnComplete(func(ag *Agent) { completedID.Store(ag.ID) })

	// PID 0 -> process gone; log shows a terminal result.
	if err := m.reg.Save(Record{ID: "comp1", TaskID: "t", Mode: "headless", Provider: "claude", PID: 0, LogPath: logPath}); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := m.ReattachAll(); len(got) != 0 {
		t.Fatalf("expected dead record not reattached as live, got %d", len(got))
	}
	if id, _ := completedID.Load().(string); id != "comp1" {
		t.Fatalf("expected onComplete for comp1 (recovered completion), got %q", id)
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatal("expected record deleted after recovery")
	}
}

// TestProcessHeadlessLine_CapturesSessionOnInit is the real regression
// guard for Phase 2: the session id must be captured (and persisted to the
// registry) on the init/system event, not only on the terminal result —
// otherwise a mid-run crash leaves no session to resume.
func TestProcessHeadlessLine_CapturesSessionOnInit(t *testing.T) {
	regDir := t.TempDir()
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	if err := m.EnableSurviveRestart(regDir); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	a := &Agent{ID: "cap1", TaskID: "t", Mode: "headless", Provider: "claude", StartedAt: time.Now().UTC()}
	var lastEmit time.Time

	// Claude reports the session id on the init/system event (no result yet).
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-real"}`)
	if stop := m.processHeadlessLine(context.Background(), a, line, &lastEmit, "claude"); stop {
		t.Fatal("init event must not stop the stream")
	}

	if a.GetSessionID() != "sess-real" {
		t.Fatalf("expected session captured on init, got %q", a.GetSessionID())
	}
	list, _ := m.reg.List()
	if len(list) != 1 || list[0].SessionID != "sess-real" {
		t.Fatalf("expected registry record carrying session id, got %+v", list)
	}
}

// TestReattachAll_BridgesDeadSessionForResume verifies a crashed headless
// agent (process gone, log has no terminal result) has its captured session
// id bridged to its AgentRun via the sink, so restart-stale can resume.
func TestReattachAll_BridgesDeadSessionForResume(t *testing.T) {
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "crash1.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Assistant output but NO terminal result -> genuine crash mid-run.
	partial := `{"type":"assistant","message":{"content":[{"type":"text","text":"half"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(partial), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	if err := m.EnableSurviveRestart(t.TempDir()); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	var gotTask, gotAgent, gotSession atomic.Value
	m.SetSessionSink(func(taskID, agentID, sessionID string) error {
		gotTask.Store(taskID)
		gotAgent.Store(agentID)
		gotSession.Store(sessionID)
		return nil
	})

	// Dead process (PID 0), session captured in the registry record.
	if err := m.reg.Save(Record{ID: "crash1", TaskID: "t9", Mode: "headless", Provider: "claude", PID: 0, LogPath: logPath, SessionID: "sess-crash"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := m.ReattachAll(); len(got) != 0 {
		t.Fatalf("expected crashed record not reattached as live, got %d", len(got))
	}
	if tid, _ := gotTask.Load().(string); tid != "t9" {
		t.Fatalf("expected sink taskID t9, got %q", tid)
	}
	if aid, _ := gotAgent.Load().(string); aid != "crash1" {
		t.Fatalf("expected sink agentID crash1, got %q", aid)
	}
	if sid, _ := gotSession.Load().(string); sid != "sess-crash" {
		t.Fatalf("expected sink sessionID sess-crash, got %q", sid)
	}
	if list, _ := m.reg.List(); len(list) != 0 {
		t.Fatal("expected record deleted after bridge")
	}
}

// TestReattachAll_RetainsRecordWhenBridgeFails verifies a sink error keeps
// the registry record (for a later retry) instead of deleting it and losing
// the session id.
func TestReattachAll_RetainsRecordWhenBridgeFails(t *testing.T) {
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "crash2.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir)
	if err := m.EnableSurviveRestart(t.TempDir()); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}
	m.SetSessionSink(func(_, _, _ string) error { return fmt.Errorf("task store down") })

	if err := m.reg.Save(Record{ID: "crash2", TaskID: "t", Mode: "headless", Provider: "claude", PID: 0, LogPath: logPath, SessionID: "s"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	m.ReattachAll()

	list, _ := m.reg.List()
	if len(list) != 1 {
		t.Fatalf("expected record retained on bridge failure, got %d", len(list))
	}
}

// TestShutdownWithGrace_LeavesDetachedAgents verifies a detached agent is
// neither cancelled nor waited on, while a normal agent is cancelled.
func TestShutdownWithGrace_LeavesDetachedAgents(t *testing.T) {
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())

	var normalCancelled atomic.Bool
	normal := &Agent{ID: "n1", Mode: "headless", done: make(chan struct{}), cancel: func() { normalCancelled.Store(true) }}
	// Normal agent's goroutine would close done on cancel; emulate it.
	go func() {
		for !normalCancelled.Load() {
			time.Sleep(5 * time.Millisecond)
		}
		close(normal.done)
	}()

	var detachedCancelled atomic.Bool
	detached := &Agent{ID: "d1", Mode: "headless", detached: true, done: make(chan struct{}), cancel: func() { detachedCancelled.Store(true) }}

	m.mu.Lock()
	m.agents["n1"] = normal
	m.agents["d1"] = detached
	m.mu.Unlock()

	m.ShutdownWithGrace(2 * time.Second)

	if !normalCancelled.Load() {
		t.Fatal("expected normal agent to be cancelled")
	}
	if detachedCancelled.Load() {
		t.Fatal("expected detached agent to be left running (not cancelled)")
	}
}
