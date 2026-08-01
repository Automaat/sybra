package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// invariantSnapshot atomically reads liveCount and sum(liveByProvider) under
// the same lock, so a concurrently running reattach/completion goroutine
// cannot skew the comparison between two independent lock acquisitions.
func invariantSnapshot(m *Manager) (sum, liveCount int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.liveByProvider {
		sum += v
	}
	return sum, m.liveCount
}

func assertAccountingInvariant(t *testing.T, m *Manager) {
	t.Helper()
	sum, liveCount := invariantSnapshot(m)
	if sum != liveCount {
		t.Fatalf("sum(liveByProvider)=%d != liveCount=%d", sum, liveCount)
	}
}

// TestReattachAccountingInvariant exercises the headless ReattachAll path
// and asserts liveByProvider never drifts from liveCount. Reattach
// increments liveCount directly (mirroring registerRunningAgent) and must
// co-locate a matching liveByProvider increment, or the soft per-provider
// dispatch cap silently desyncs after a restart that finds in-flight agents.
func TestReattachAccountingInvariant(t *testing.T) {
	prev := reattachPIDPoll.Load()
	reattachPIDPoll.Store((50 * time.Millisecond).Nanoseconds())
	t.Cleanup(func() { reattachPIDPoll.Store(prev) })

	logDir := t.TempDir()
	regDir := t.TempDir()
	agentsLogDir := filepath.Join(logDir, "agents")
	if err := os.MkdirAll(agentsLogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete:        func(*Agent) {},
	})
	assertAccountingInvariant(t, m)

	spawnSleeper := func(t *testing.T) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("sleep", "2")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper process: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		go func() { _ = cmd.Wait() }()
		return cmd
	}

	// Headless path: registered via the registry and reattached through the
	// full ReattachAll sweep (mirrors app startup).
	headlessLog := filepath.Join(agentsLogDir, "h1.ndjson")
	if err := os.WriteFile(headlessLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	headlessCmd := spawnSleeper(t)
	if err := m.reg.Save(Record{
		ID:            "h1",
		TaskID:        "task-h",
		Mode:          "headless",
		Provider:      "claude",
		PID:           headlessCmd.Process.Pid,
		LogPath:       headlessLog,
		StartedAt:     time.Now().UTC(),
		ProcStartedAt: processStartString(context.Background(), headlessCmd.Process.Pid),
	}); err != nil {
		t.Fatal(err)
	}

	reattached := m.ReattachAll()
	if len(reattached) != 1 {
		t.Fatalf("ReattachAll: got %d reattached agents, want 1", len(reattached))
	}
	assertAccountingInvariant(t, m)

	// Kill the live helper so the agent runs to completion, then confirm the
	// invariant still holds once liveCount/liveByProvider both drain.
	_ = headlessCmd.Process.Kill()

	deadline := time.After(5 * time.Second)
	for m.RunningCount() > 0 {
		select {
		case <-deadline:
			t.Fatalf("agents did not drain to 0 within 5s (liveCount=%d, liveByProvider=%+v)",
				m.RunningCount(), m.InFlightByProvider())
		case <-time.After(20 * time.Millisecond):
		}
	}
	assertAccountingInvariant(t, m)
	if got := m.InFlightByProvider(); len(got) != 0 {
		t.Fatalf("expected empty in-flight map after full drain, got %+v", got)
	}
}

// TestReattachReapsStaleHeadlessTaskAgentButKeepsOrchestrator guards the
// RoleOrchestrator carve-out in reattachStaleReason: the orchestrator brain
// is a deliberately taskless, long-lived headless agent (svc_orchestrator.go
// dispatches it with Mode: "headless" and no TaskID) and must survive a
// restart like any live process — including past reattachMaxAge — while an
// ordinary headless task agent that is both past reattachMaxAge AND stuck
// (no log progress) is still reaped.
func TestReattachReapsStaleHeadlessTaskAgentButKeepsOrchestrator(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	prevGrace := stopSIGINTGrace
	stopSIGINTGrace = 30 * time.Millisecond
	t.Cleanup(func() {
		stopSIGINTGrace = prevGrace
	})
	spawnSleeper := func(t *testing.T) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleeper: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		go func() { _ = cmd.Wait() }()
		return cmd
	}

	logDir := t.TempDir()
	agentsLogDir := filepath.Join(logDir, "agents")
	if err := os.MkdirAll(agentsLogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	regDir := t.TempDir()
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete:        func(*Agent) {},
	})

	logFor := func(id string) string {
		p := filepath.Join(agentsLogDir, id+".ndjson")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	staleCmd := spawnSleeper(t)
	orchCmd := spawnSleeper(t)
	records := []Record{
		{
			// No log progress (writeStaleLog backdates the mtime), so this
			// is genuinely stuck, not merely old — see the liveness+progress
			// policy in reattachStaleReason.
			ID: "task-agent", TaskID: "task-x", Mode: "headless", Provider: "claude",
			PID: staleCmd.Process.Pid, LogPath: writeStaleLog(t, logDir, "task-agent"),
			StartedAt:     time.Now().Add(-24 * time.Hour).UTC(),
			ProcStartedAt: processStartString(context.Background(), staleCmd.Process.Pid),
		},
		{
			ID: "orchestrator", Mode: "headless", Provider: "claude", Role: RoleOrchestrator,
			PID: orchCmd.Process.Pid, LogPath: logFor("orchestrator"),
			StartedAt:     time.Now().Add(-24 * time.Hour).UTC(),
			ProcStartedAt: processStartString(context.Background(), orchCmd.Process.Pid),
		},
	}
	for _, r := range records {
		if err := m.reg.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	m.ReattachAll()

	recs, err := m.reg.List()
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, r := range recs {
		present[r.ID] = true
	}
	if present["task-agent"] {
		t.Errorf("stale headless task agent must be reaped, but its record survives")
	}
	if !present["orchestrator"] {
		t.Errorf("taskless orchestrator must NOT be reaped, but its record is gone")
	}
	if err := orchCmd.Process.Kill(); err != nil {
		t.Fatalf("kill reattached orchestrator process: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for m.RunningCount() > 0 {
		select {
		case <-deadline:
			t.Fatalf("reattached orchestrator did not stop; liveCount=%d", m.RunningCount())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestReattachReapsStaleSurvivors(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	prevGrace := stopSIGINTGrace
	stopSIGINTGrace = 30 * time.Millisecond
	t.Cleanup(func() {
		stopSIGINTGrace = prevGrace
	})

	spawnSleeper := func(t *testing.T) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper process: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		go func() { _ = cmd.Wait() }()
		return cmd
	}

	// Every case below represents a "dead" (not progressing) survivor: its
	// log file, when present, is backdated well past reattachStuckAfter, so
	// these cases exercise (parked × dead) / (gone × dead), not the
	// liveness+progress carve-out covered by TestReattachAdoptsHealthySurvivor
	// and TestReattachAdoptsParkedOrOldProgressingSurvivor.
	tests := []struct {
		name        string
		record      func(pid int) Record
		status      map[string]string
		taskDeleted bool
		withLog     bool
	}{
		{
			name: "empty task id",
			record: func(pid int) Record {
				return Record{ID: "s1", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().UTC()}
			},
		},
		{
			name: "past wall-clock deadline, no progress",
			record: func(pid int) Record {
				return Record{ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().Add(-24 * time.Hour).UTC()}
			},
			withLog: true,
		},
		{
			name: "parked (todo) task, dead survivor",
			record: func(pid int) Record {
				return Record{ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().UTC()}
			},
			status:  map[string]string{"task-x": "todo"},
			withLog: true,
		},
		{
			name: "parked (human-required) task, dead survivor",
			record: func(pid int) Record {
				return Record{ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().UTC()}
			},
			status:  map[string]string{"task-x": "human-required"},
			withLog: true,
		},
		{
			name: "parked (blocked) task, dead survivor",
			record: func(pid int) Record {
				return Record{ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().UTC()}
			},
			status:  map[string]string{"task-x": "blocked"},
			withLog: true,
		},
		{
			name: "parked (done) task, dead survivor",
			record: func(pid int) Record {
				return Record{ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().UTC()}
			},
			status:  map[string]string{"task-x": "done"},
			withLog: true,
		},
		{
			name: "task deleted while down",
			record: func(pid int) Record {
				return Record{ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().UTC()}
			},
			taskDeleted: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logDir := t.TempDir()
			regDir := t.TempDir()
			cfg := ManagerConfig{SurviveRestartDir: regDir, OnComplete: func(*Agent) {}}
			if tc.status != nil {
				st := tc.status
				cfg.TaskStatus = func(id string) (string, bool) { s, ok := st[id]; return s, ok }
			}
			if tc.taskDeleted {
				cfg.TaskExists = func(string) bool { return false }
			}
			m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, cfg)

			cmd := spawnSleeper(t)
			rec := tc.record(cmd.Process.Pid)
			rec.ProcStartedAt = processStartString(context.Background(), cmd.Process.Pid)
			if tc.withLog {
				rec.LogPath = writeStaleLog(t, logDir, "s1")
			}
			if err := m.reg.Save(rec); err != nil {
				t.Fatal(err)
			}

			reattached := m.ReattachAll()
			if len(reattached) != 0 {
				t.Fatalf("ReattachAll adopted %d stale agents, want 0", len(reattached))
			}
			if m.RunningCount() != 0 {
				t.Fatalf("liveCount = %d, want 0 (a reaped survivor must not hold a pool slot)", m.RunningCount())
			}
			recs, err := m.reg.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 0 {
				t.Fatalf("stale survivor record not reaped: %d remain", len(recs))
			}
		})
	}
}

func TestReattachAdoptsHealthySurvivor(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())

	logDir := t.TempDir()
	regDir := t.TempDir()
	agentsLogDir := filepath.Join(logDir, "agents")
	if err := os.MkdirAll(agentsLogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
		SurviveRestartDir: regDir,
		OnComplete:        func(*Agent) {},
		TaskStatus:        func(string) (string, bool) { return "in-progress", true },
	})

	logPath := filepath.Join(agentsLogDir, "h1.ndjson")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	go func() { _ = cmd.Wait() }()

	if err := m.reg.Save(Record{
		ID: "h1", TaskID: "task-h", Mode: "headless", Provider: "claude",
		PID: cmd.Process.Pid, LogPath: logPath, StartedAt: time.Now().UTC(),
		ProcStartedAt: processStartString(context.Background(), cmd.Process.Pid),
	}); err != nil {
		t.Fatal(err)
	}

	reattached := m.ReattachAll()
	if len(reattached) != 1 {
		t.Fatalf("ReattachAll adopted %d healthy agents, want 1", len(reattached))
	}
	if m.RunningCount() != 1 {
		t.Fatalf("liveCount = %d, want 1", m.RunningCount())
	}
	assertAccountingInvariant(t, m)
}

// writeStaleLog writes an empty NDJSON log for id under dir/agents and
// backdates its mtime past reattachStuckAfter, so reattachProgressing reads
// it as a stuck (not progressing) survivor.
func writeStaleLog(t *testing.T, dir, id string) string {
	t.Helper()
	agentsLogDir := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agentsLogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(agentsLogDir, id+".ndjson")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * reattachStuckAfter)
	if err := os.Chtimes(p, stale, stale); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeFreshLog writes an empty NDJSON log for id under dir/agents with a
// current mtime, so reattachProgressing reads it as progressing.
func writeFreshLog(t *testing.T, dir, id string) string {
	t.Helper()
	agentsLogDir := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agentsLogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(agentsLogDir, id+".ndjson")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReattachAdoptsParkedOrOldProgressingSurvivor is the core regression
// coverage for the reattach fix: a healthy (progressing) survivor must be
// adopted, not killed, whether its task was parked while the app was down or
// it has simply been running longer than reattachMaxAge. Pairs with the
// (parked × dead) cases in TestReattachReapsStaleSurvivors and the
// (old × not progressing) "past wall-clock deadline" case there.
func TestReattachAdoptsParkedOrOldProgressingSurvivor(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())

	prevPoll := reattachPIDPoll.Load()
	reattachPIDPoll.Store((20 * time.Millisecond).Nanoseconds())
	t.Cleanup(func() { reattachPIDPoll.Store(prevPoll) })

	spawnSleeper := func(t *testing.T) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper process: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		go func() { _ = cmd.Wait() }()
		return cmd
	}

	tests := []struct {
		name      string
		startedAt time.Time
		status    string
	}{
		{
			name:      "parked (todo) task, healthy survivor",
			startedAt: time.Now().UTC(),
			status:    "todo",
		},
		{
			name:      "parked (human-required) task, healthy survivor",
			startedAt: time.Now().UTC(),
			status:    "human-required",
		},
		{
			name:      "past wall-clock deadline, still progressing",
			startedAt: time.Now().Add(-24 * time.Hour).UTC(),
			status:    "in-progress",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logDir := t.TempDir()
			regDir := t.TempDir()
			status := tc.status
			m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), logDir, ManagerConfig{
				SurviveRestartDir: regDir,
				OnComplete:        func(*Agent) {},
				TaskStatus:        func(string) (string, bool) { return status, true },
			})

			cmd := spawnSleeper(t)
			logPath := writeFreshLog(t, logDir, "s1")
			rec := Record{
				ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude",
				PID: cmd.Process.Pid, LogPath: logPath, StartedAt: tc.startedAt,
				ProcStartedAt: processStartString(context.Background(), cmd.Process.Pid),
			}
			if err := m.reg.Save(rec); err != nil {
				t.Fatal(err)
			}

			reattached := m.ReattachAll()
			if len(reattached) != 1 {
				t.Fatalf("ReattachAll adopted %d survivors, want 1 (healthy survivor must not be reaped)", len(reattached))
			}
			if m.RunningCount() != 1 {
				t.Fatalf("liveCount = %d, want 1", m.RunningCount())
			}
			recs, err := m.reg.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 1 {
				t.Fatalf("expected adopted survivor's record to remain, got %d", len(recs))
			}
			assertAccountingInvariant(t, m)

			// Drain before the subtest ends so the tailer/watchPID goroutines
			// spawned by ReattachAll exit deterministically instead of racing
			// t.Cleanup's process kill against the package's goroutine-leak check.
			_ = cmd.Process.Kill()
			deadline := time.After(5 * time.Second)
			for m.RunningCount() > 0 {
				select {
				case <-deadline:
					t.Fatalf("adopted survivor did not drain; liveCount=%d", m.RunningCount())
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
	}
}
