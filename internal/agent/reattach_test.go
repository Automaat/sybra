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
	prevPoll := reattachPIDPoll
	reattachPIDPoll = 30 * time.Millisecond
	t.Cleanup(func() { reattachPIDPoll = prevPoll })

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
		ProcStartedAt: processStartString(headlessCmd.Process.Pid),
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
		ProcStartedAt: processStartString(interactiveCmd.Process.Pid),
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
