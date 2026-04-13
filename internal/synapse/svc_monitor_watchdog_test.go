package synapse

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/events"
)

func newTestWatchdog(t *testing.T, path string) (*MonitorWatchdog, *testEmit, *atomic.Int32) {
	t.Helper()
	emitter := &testEmit{}
	var recoverCount atomic.Int32
	wd := NewMonitorWatchdog(
		path,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		emitter.emit,
		func() { recoverCount.Add(1) },
	)
	return wd, emitter, &recoverCount
}

type testEmit struct {
	events []string
	last   MonitorStatus
}

func (e *testEmit) emit(name string, data any) {
	e.events = append(e.events, name)
	if s, ok := data.(MonitorStatus); ok {
		e.last = s
	}
}

func writeHeartbeat(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestMonitorWatchdog_FreshHeartbeatIsAlive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor-heartbeat")
	writeHeartbeat(t, path, 30*time.Second)

	wd, emitter, recoverCount := newTestWatchdog(t, path)
	wd.check()

	status := wd.Status()
	if status.Stale {
		t.Errorf("fresh heartbeat reported stale: %+v", status)
	}
	if !status.Present {
		t.Errorf("fresh heartbeat reported absent: %+v", status)
	}
	if status.AgeSeconds < 20 || status.AgeSeconds > 60 {
		t.Errorf("unexpected age: %d", status.AgeSeconds)
	}
	if got := recoverCount.Load(); got != 0 {
		t.Errorf("recover called for fresh heartbeat: %d", got)
	}
	if len(emitter.events) != 1 || emitter.events[0] != events.MonitorHeartbeat {
		t.Errorf("expected one monitor:heartbeat event, got %v", emitter.events)
	}
}

func TestMonitorWatchdog_StaleFileTriggersRecoveryOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor-heartbeat")
	writeHeartbeat(t, path, 30*time.Minute)

	wd, _, recoverCount := newTestWatchdog(t, path)
	wd.check()

	if !wd.Status().Stale {
		t.Errorf("old heartbeat not flagged stale: %+v", wd.Status())
	}
	if got := recoverCount.Load(); got != 1 {
		t.Errorf("expected 1 recovery, got %d", got)
	}

	// Second tick with still-stale mtime must not re-fire recovery.
	wd.check()
	if got := recoverCount.Load(); got != 1 {
		t.Errorf("expected recovery throttled, got %d calls", got)
	}
}

func TestMonitorWatchdog_MissingFileIsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor-heartbeat")

	wd, _, recoverCount := newTestWatchdog(t, path)
	wd.check()

	status := wd.Status()
	if !status.Stale {
		t.Errorf("missing heartbeat not flagged stale: %+v", status)
	}
	if status.Present {
		t.Errorf("missing heartbeat reported present: %+v", status)
	}
	if got := recoverCount.Load(); got != 1 {
		t.Errorf("expected 1 recovery for missing file, got %d", got)
	}
}

func TestMonitorWatchdog_StaleToAliveTransition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor-heartbeat")
	writeHeartbeat(t, path, 30*time.Minute)

	wd, _, recoverCount := newTestWatchdog(t, path)
	wd.check()
	if got := recoverCount.Load(); got != 1 {
		t.Fatalf("setup: expected 1 recovery, got %d", got)
	}

	// New cycle writes a fresh heartbeat — watchdog should flip back to
	// alive without calling recover again.
	writeHeartbeat(t, path, 10*time.Second)
	wd.check()
	if wd.Status().Stale {
		t.Errorf("fresh heartbeat after recovery still stale: %+v", wd.Status())
	}
	if got := recoverCount.Load(); got != 1 {
		t.Errorf("alive transition called recover: %d", got)
	}
}

func TestMonitorWatchdog_RecoveryCooldown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor-heartbeat")
	writeHeartbeat(t, path, 30*time.Minute)

	wd, _, recoverCount := newTestWatchdog(t, path)
	wd.recoveryCD = 100 * time.Millisecond

	base := time.Now()
	wd.now = func() time.Time { return base }
	wd.check()
	if got := recoverCount.Load(); got != 1 {
		t.Fatalf("first stale: expected 1 recovery, got %d", got)
	}

	// Simulate alive→stale flap inside cooldown window.
	writeHeartbeat(t, path, 10*time.Second)
	wd.now = func() time.Time { return base.Add(10 * time.Millisecond) }
	wd.check()
	if wd.Status().Stale {
		t.Fatalf("flap to alive failed: %+v", wd.Status())
	}
	writeHeartbeat(t, path, 30*time.Minute)
	wd.now = func() time.Time { return base.Add(20 * time.Millisecond) }
	wd.check()
	if got := recoverCount.Load(); got != 1 {
		t.Errorf("cooldown ignored — expected 1 recovery, got %d", got)
	}

	// After cooldown elapses, another stale transition should recover.
	wd.now = func() time.Time { return base.Add(200 * time.Millisecond) }
	wd.check()
	if got := recoverCount.Load(); got != 2 {
		t.Errorf("post-cooldown recovery missing — got %d", got)
	}
}
