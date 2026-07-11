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

// TestReattachAccountingInvariant exercises all three restart-reattach code
// paths (headless via ReattachAll, reattachInteractive, reattachPerTurnConvo)
// and asserts liveByProvider never drifts from liveCount. Each of these
// paths increments liveCount directly (mirroring registerRunningAgent) and
// must co-locate a matching liveByProvider increment, or the soft
// per-provider dispatch cap silently desyncs after every restart that finds
// in-flight agents.
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

	// --- Headless path: registered via the registry and reattached through
	// the full ReattachAll sweep (mirrors app startup). ---
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

	// --- Interactive path: reattachInteractive directly (line ~225 site). ---
	interactiveLog := filepath.Join(agentsLogDir, "i1.ndjson")
	if err := os.WriteFile(interactiveLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	interactiveCmd := spawnSleeper(t)
	reg := m.registry()
	interactiveAgent := m.reattachInteractive(Record{
		ID:            "i1",
		TaskID:        "task-i",
		Mode:          "interactive",
		Provider:      "claude",
		PID:           interactiveCmd.Process.Pid,
		LogPath:       interactiveLog,
		StartedAt:     time.Now().UTC(),
		ProcStartedAt: processStartString(context.Background(), interactiveCmd.Process.Pid),
	}, reg)
	if interactiveAgent == nil {
		t.Fatal("reattachInteractive returned nil, expected a live agent")
	}
	assertAccountingInvariant(t, m)

	// --- Per-turn-convo path: reattachPerTurnConvo directly (line ~291 site). ---
	perTurnAgent := m.reattachPerTurnConvo(Record{
		ID:       "p1",
		TaskID:   "task-p",
		Mode:     "interactive",
		Provider: "codex",
		OneShot:  false,
	}, reg)
	if perTurnAgent == nil {
		t.Fatal("reattachPerTurnConvo returned nil, expected a live agent")
		return
	}
	assertAccountingInvariant(t, m)

	// Kill the live helpers so every agent runs to completion, then confirm
	// the invariant still holds once liveCount/liveByProvider both drain.
	_ = headlessCmd.Process.Kill()
	_ = interactiveCmd.Process.Kill()
	_ = m.StopAgent(perTurnAgent.ID)

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

func TestReattachReapsStaleInteractiveTaskAgent(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
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
			ID: "task-agent", TaskID: "task-x", Mode: "interactive", Provider: "claude",
			PID: staleCmd.Process.Pid, LogPath: logFor("task-agent"),
			StartedAt:     time.Now().Add(-24 * time.Hour).UTC(),
			ProcStartedAt: processStartString(context.Background(), staleCmd.Process.Pid),
		},
		{
			ID: "orchestrator", Mode: "interactive", Provider: "claude",
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
		t.Errorf("stale interactive task agent must be reaped, but its record survives")
	}
	if !present["orchestrator"] {
		t.Errorf("taskless orchestrator must NOT be reaped, but its record is gone")
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

	tests := []struct {
		name        string
		record      func(pid int) Record
		status      map[string]string
		taskDeleted bool
	}{
		{
			name: "empty task id",
			record: func(pid int) Record {
				return Record{ID: "s1", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().UTC()}
			},
		},
		{
			name: "past wall-clock deadline",
			record: func(pid int) Record {
				return Record{ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().Add(-24 * time.Hour).UTC()}
			},
		},
		{
			name: "inconsistent task status",
			record: func(pid int) Record {
				return Record{ID: "s1", TaskID: "task-x", Mode: "headless", Provider: "claude", PID: pid, StartedAt: time.Now().UTC()}
			},
			status: map[string]string{"task-x": "todo"},
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
