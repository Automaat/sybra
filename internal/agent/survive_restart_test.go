package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

func TestAgentRecordMappingRoundTrip(t *testing.T) {
	started := recordMappingStartedAt()
	a := recordMappingAgent(started)
	a.sessionCWD = "/tmp/sybra/worktrees/task-map"
	a.sandboxHomeDir = "/tmp/sybra/sandboxes/task-map"
	a.setStdinPath("/tmp/sybra/agents/a-map.stdin")
	a.oneShot = true
	a.requirePermissions = true
	a.sandboxMode = "enforce"
	a.EnqueuePrompt("queued turn 1")
	a.EnqueuePrompt("queued turn 2")

	want := recordMappingRecord(started)
	assertRecordFixtureCoversFields(t, want, "ProcStartedAt")

	if got := a.toRecord(); !reflect.DeepEqual(got, want) {
		t.Fatalf("toRecord mismatch:\n got: %+v\nwant: %+v", got, want)
	}

	before := time.Now().UTC()
	restored := fromRecord(want)
	after := time.Now().UTC()
	if got := restored.toRecord(); !reflect.DeepEqual(got, want) {
		t.Fatalf("fromRecord/toRecord mismatch:\n got: %+v\nwant: %+v", got, want)
	}
	if got := restored.GetState(); got != StateRunning {
		t.Fatalf("fromRecord must force running state, got %s", got)
	}
	last := restored.GetLastEventAt()
	if last.Before(before) || last.After(after) {
		t.Fatalf("fromRecord must refresh LastEventAt, got %s outside [%s, %s]", last, before, after)
	}
	if !restored.isDetached() {
		t.Fatal("fromRecord must mark the skeleton agent detached")
	}
	if restored.cancel != nil || restored.done != nil || restored.hasPromptChannel() || restored.GetCmd() != nil {
		t.Fatal("fromRecord must leave live runtime wiring to reattach callers")
	}

	withProcStart := want
	withProcStart.ProcStartedAt = "Mon Jun 28 20:30:00 2026"
	if got := fromRecord(withProcStart).toRecord(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProcStartedAt must stay caller-owned:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestRegistryStore_CurrentYAMLFixture(t *testing.T) {
	want := recordMappingRecord(recordMappingStartedAt())
	want.ProcStartedAt = "Mon Jun 28 20:30:00 2026"

	var got Record
	raw := readRegistryFixture(t, "registry_current.yaml")
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal current registry fixture: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("current registry fixture mismatch:\n got: %+v\nwant: %+v", got, want)
	}

	marshaled, err := yaml.Marshal(want)
	if err != nil {
		t.Fatalf("marshal current registry fixture: %v", err)
	}
	if strings.TrimSpace(string(marshaled)) != strings.TrimSpace(string(raw)) {
		t.Fatalf("current registry YAML drift:\n got:\n%s\nwant:\n%s", marshaled, raw)
	}
}

func TestRegistryStore_LegacyYAMLFixture(t *testing.T) {
	wantStartedAt := recordMappingStartedAt()
	want := Record{
		ID:            "legacy-agent",
		TaskID:        "legacy-task",
		Mode:          "headless",
		Provider:      "claude",
		PID:           23456,
		SessionID:     "legacy-session",
		LogPath:       "/tmp/sybra/agents/legacy-agent.ndjson",
		CWD:           "/tmp/sybra/worktrees/legacy-task",
		StartedAt:     wantStartedAt,
		ProcStartedAt: "Mon Jun 28 20:30:00 2026",
	}

	var got Record
	if err := yaml.Unmarshal(readRegistryFixture(t, "registry_legacy.yaml"), &got); err != nil {
		t.Fatalf("unmarshal legacy registry fixture: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy registry fixture mismatch:\n got: %+v\nwant: %+v", got, want)
	}

	restored := fromRecord(got)
	if restored.ID != want.ID || restored.TaskID != want.TaskID || restored.Mode != want.Mode ||
		restored.Provider != want.Provider || restored.PID != want.PID ||
		restored.SessionID != want.SessionID || restored.LogPath != want.LogPath ||
		restored.sessionCWD != want.CWD || !restored.StartedAt.Equal(wantStartedAt) {
		t.Fatalf("legacy registry reattach skeleton mismatch: %+v", restored)
	}
}

func recordMappingStartedAt() time.Time {
	return time.Date(2026, 6, 28, 20, 30, 0, 0, time.UTC)
}

func recordMappingAgent(started time.Time) *Agent {
	return &Agent{
		ID:                 "a-map",
		TaskID:             "task-map",
		Name:               "mapper",
		Mode:               "interactive",
		Provider:           "codex",
		Model:              "gpt-5.3-codex",
		ExperimentID:       "exp-1",
		VariantID:          "variant-a",
		AssignmentUnit:     "task",
		AssignmentKey:      "task-map",
		PID:                12345,
		SessionID:          "sess-map",
		LogPath:            "/tmp/sybra/agents/a-map.ndjson",
		StartedAt:          started,
		MaxTurns:           7,
		ReasoningEffort:    "high",
		SkillExecutionMode: "injected",
	}
}

func recordMappingRecord(started time.Time) Record {
	return Record{
		ID:                 "a-map",
		TaskID:             "task-map",
		Name:               "mapper",
		Mode:               "interactive",
		Provider:           "codex",
		Model:              "gpt-5.3-codex",
		ExperimentID:       "exp-1",
		VariantID:          "variant-a",
		AssignmentUnit:     "task",
		AssignmentKey:      "task-map",
		PID:                12345,
		SessionID:          "sess-map",
		LogPath:            "/tmp/sybra/agents/a-map.ndjson",
		CWD:                "/tmp/sybra/worktrees/task-map",
		SandboxHomeDir:     "/tmp/sybra/sandboxes/task-map",
		StartedAt:          started,
		StdinPath:          "/tmp/sybra/agents/a-map.stdin",
		PendingPrompts:     []string{"queued turn 1", "queued turn 2"},
		OneShot:            true,
		MaxTurns:           7,
		RequirePermissions: true,
		SandboxMode:        "enforce",
		ReasoningEffort:    "high",
		SkillExecutionMode: "injected",
	}
}

func readRegistryFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read registry fixture %s: %v", name, err)
	}
	return raw
}

func assertRecordFixtureCoversFields(t *testing.T, rec Record, ignore ...string) {
	t.Helper()
	ignored := map[string]struct{}{}
	for _, field := range ignore {
		ignored[field] = struct{}{}
	}
	value := reflect.ValueOf(rec)
	for _, field := range reflect.VisibleFields(value.Type()) {
		if _, ok := ignored[field.Name]; ok {
			continue
		}
		if value.FieldByIndex(field.Index).IsZero() {
			t.Fatalf("record mapping test fixture does not cover Record.%s", field.Name)
		}
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
	start := processStartString(context.Background(), self)
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
	prev := reattachPIDPoll.Load()
	reattachPIDPoll.Store((50 * time.Millisecond).Nanoseconds())
	t.Cleanup(func() { reattachPIDPoll.Store(prev) })

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

	var completed atomic.Bool
	var completedID atomic.Value
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete: func(ag *Agent) {
			completedID.Store(ag.ID)
			completed.Store(true)
		},
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
		ProcStartedAt: processStartString(context.Background(), cmd.Process.Pid),
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
	deadline := time.After(scaledDeadline(5 * time.Second))
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
	if found, isError := a.lastHeadlessResult(); !found || isError {
		t.Fatal("expected terminal result in buffer")
	}
	waitForRegistryEmpty(t, m, 5*time.Second)
}

func TestManagerRunPersistsAndReattachesLiveHeadlessAgent(t *testing.T) {
	// Exercises the full Run -> persisted registry record -> fresh manager
	// ReattachAll path; sibling reattach tests start from injected records.
	prev := reattachPIDPoll.Load()
	reattachPIDPoll.Store((50 * time.Millisecond).Nanoseconds())
	t.Cleanup(func() { reattachPIDPoll.Store(prev) })

	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	if err := os.WriteFile(fakeClaude, []byte(`#!/bin/sh
trap 'exit 130' INT TERM
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-lifecycle"}'
i=0
while [ "$i" -lt 5 ]; do
  sleep 0.1
  i=$((i + 1))
done
printf '%s\n' '{"type":"result","result":"done","session_id":"sess-lifecycle","total_cost_usd":0}'
`), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	logDir := t.TempDir()
	regDir := t.TempDir()
	runDir := t.TempDir()

	ctx1, cancel1 := context.WithCancel(context.Background())
	m1 := mustNewManager(t, ctx1, func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		Runtime:           ManagerRuntimeConfig{DefaultProvider: "claude"},
		SurviveRestartDir: regDir,
	})
	ag, err := m1.Run(RunConfig{
		TaskID:             "task-lifecycle",
		Name:               "implementation: lifecycle",
		Mode:               "headless",
		Prompt:             "exercise lifecycle",
		Dir:                runDir,
		RequirePermissions: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec := waitForRegistryRecord(t, m1, ag.ID)
	if rec.TaskID != "task-lifecycle" || rec.Mode != "headless" || rec.Provider != "claude" {
		t.Fatalf("unexpected persisted record: %+v", rec)
	}
	if rec.PID == 0 || rec.LogPath == "" || rec.CWD != runDir {
		t.Fatalf("record missing real process boundary fields: %+v", rec)
	}
	if rec.ProcStartedAt == "" {
		t.Fatalf("record missing PID reuse guard: %+v", rec)
	}
	if rec.SessionID != "sess-lifecycle" {
		t.Fatalf("record session = %q, want sess-lifecycle", rec.SessionID)
	}

	// Model an app restart: cancel the first manager's root context but leave
	// the detached child alive, then construct a fresh manager over the same
	// registry and log directories.
	cancel1()

	m2 := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		Runtime:           ManagerRuntimeConfig{DefaultProvider: "claude"},
		SurviveRestartDir: regDir,
	})
	reattached := m2.ReattachAll()
	if len(reattached) != 1 {
		t.Fatalf("expected 1 reattached agent, got %d", len(reattached))
	}
	got := reattached[0]
	if got.ID != ag.ID || got.TaskID != "task-lifecycle" || got.GetPID() != rec.PID || got.LogPath != rec.LogPath {
		t.Fatalf("reattached agent mismatch:\n got: %+v\nrecord: %+v", got, rec)
	}
	if got.GetState() != StateRunning {
		t.Fatalf("reattached state = %s, want %s", got.GetState(), StateRunning)
	}
	if got.GetSessionID() != "sess-lifecycle" {
		t.Fatalf("reattached session = %q, want sess-lifecycle", got.GetSessionID())
	}
	if len(got.Output()) == 0 {
		t.Fatal("expected reattached agent to rehydrate output from log")
	}

	waitForAgentDone(t, got, 5*time.Second)
	if got.GetState() != StateStopped {
		t.Fatalf("completed reattached state = %s, want %s", got.GetState(), StateStopped)
	}
	if got.GetExitErr() != nil {
		t.Fatalf("completed reattached agent has exit error: %v", got.GetExitErr())
	}
}

// TestReattachAll_DropsDeadRecords verifies a record whose process is gone
// is deleted and not reattached.
func TestReattachAll_DropsDeadRecords(t *testing.T) {
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{SurviveRestartDir: t.TempDir()})
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
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
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
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
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

func TestTailHeadlessFile_ReattachedFastCloseDoesNotWaitFullGrace(t *testing.T) {
	prevDrain := drainTimeout
	drainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { drainTimeout = prevDrain })

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	logPath := filepath.Join(t.TempDir(), "tail-fast-close.ndjson")
	content := `{"type":"result","result":"done","session_id":"sess-1","total_cost_usd":0}` + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	a := &Agent{
		ID:                   "fast-close",
		Provider:             "claude",
		postResultWaitReason: postResultWaitFastClose,
		postResultWaitSince:  time.Now().Add(-time.Second),
	}
	procDone := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(procDone)
	}()

	start := time.Now()
	exited, _ := m.tailHeadlessFile(context.Background(), a, logPath, 0, procDone)
	if !exited {
		t.Fatal("expected exited=true on persisted fast-close state")
	}
	if elapsed := time.Since(start); elapsed >= postResultGrace/2 {
		t.Fatalf("tailHeadlessFile took %s, want prompt close instead of waiting near postResultGrace", elapsed)
	}
	if !a.WasCompletedByResult() {
		t.Fatal("WasCompletedByResult() = false, want true on reattached fast-close path")
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

	var completedID atomic.Value
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: t.TempDir(),
		OnComplete:        func(ag *Agent) { completedID.Store(ag.ID) },
	})

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

// TestReattachAll_RecoversCompletedDuringDowntime_UsesLogMtimeForDuration
// guards the dead-process recovery path against the duration bug the fix
// targets: replaying the log stamps every event at replay time, so without
// the fix LastEventAt collapses to the reattach wall-clock moment and a run
// finalized after an app-downtime gap reports the full idle time as its
// duration. The log file's mtime is the last real signal of when the
// process actually finished, so it must win instead.
func TestReattachAll_RecoversCompletedDuringDowntime_UsesLogMtimeForDuration(t *testing.T) {
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "agents", "comp2.ndjson")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n" +
		`{"type":"result","result":"done","session_id":"sess-9","total_cost_usd":0.2}` + "\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	// Simulate the process having actually finished writing well before this
	// reattach runs (the app-downtime gap).
	finishedAt := time.Now().Add(-2 * time.Hour).UTC()
	if err := os.Chtimes(logPath, finishedAt, finishedAt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	var completed atomic.Value
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: t.TempDir(),
		OnComplete:        func(ag *Agent) { completed.Store(ag) },
	})

	startedAt := finishedAt.Add(-time.Minute)
	if err := m.reg.Save(Record{ID: "comp2", TaskID: "t", Mode: "headless", Provider: "claude", PID: 0, LogPath: logPath, StartedAt: startedAt}); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := m.ReattachAll(); len(got) != 0 {
		t.Fatalf("expected dead record not reattached as live, got %d", len(got))
	}
	ag, _ := completed.Load().(*Agent)
	if ag == nil {
		t.Fatal("expected onComplete to fire for comp2")
	}
	if got := ag.GetLastEventAt(); got.Sub(finishedAt).Abs() > time.Second {
		t.Fatalf("LastEventAt = %s, want ~%s (log mtime, not reattach wall-clock)", got, finishedAt)
	}
}

// TestProcessHeadlessLine_CapturesSessionOnInit is the real regression
// guard for Phase 2: the session id must be captured (and persisted to the
// registry) on the init/system event, not only on the terminal result —
// otherwise a mid-run crash leaves no session to resume.
func TestProcessHeadlessLine_CapturesSessionOnInit(t *testing.T) {
	regDir := t.TempDir()
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{SurviveRestartDir: regDir})
	a := &Agent{ID: "cap1", TaskID: "t", Mode: "headless", Provider: "claude", StartedAt: time.Now().UTC()}
	var lastEmit time.Time

	// Claude reports the session id on the init/system event (no result yet).
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-real"}`)
	if stop := m.processHeadlessLine(context.Background(), a, line, &lastEmit, providerByName("claude")); stop {
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

	var gotTask, gotAgent, gotSession atomic.Value
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: t.TempDir(),
		SessionSink: func(taskID, agentID, sessionID string) error {
			gotTask.Store(taskID)
			gotAgent.Store(agentID)
			gotSession.Store(sessionID)
			return nil
		},
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

func waitForRegistryRecord(t *testing.T, m *Manager, agentID string) Record {
	t.Helper()
	deadline := time.After(scaledDeadline(5 * time.Second))
	for {
		recs, err := m.reg.List()
		if err != nil {
			t.Fatalf("registry list: %v", err)
		}
		for i := range recs {
			rec := &recs[i]
			if rec.ID == agentID && rec.PID != 0 && rec.LogPath != "" && rec.SessionID != "" {
				return *rec
			}
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for registry record for %s; records=%+v", agentID, recs)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitForRegistryEmpty(t *testing.T, m *Manager, timeout time.Duration) {
	t.Helper()
	deadline := time.After(scaledDeadline(timeout))
	for {
		list, err := m.reg.List()
		if err != nil {
			t.Fatalf("registry list: %v", err)
		}
		if len(list) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected registry empty after completion, got %d", len(list))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitForAgentDone(t *testing.T, ag *Agent, timeout time.Duration) {
	t.Helper()
	if ag.done == nil {
		t.Fatal("waitForAgentDone: agent has no done channel; reattach wiring incomplete")
	}
	select {
	case <-ag.done:
	case <-time.After(scaledDeadline(timeout)):
		t.Fatalf("timed out waiting for agent %s to stop", ag.ID)
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
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: t.TempDir(),
		SessionSink:       func(_, _, _ string) error { return fmt.Errorf("task store down") },
	})

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
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())

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
