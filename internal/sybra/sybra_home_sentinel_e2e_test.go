//go:build e2e

package sybra

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
)

const defaultE2ETimeout = 10 * time.Second

// TestSybraHomeSentinel_DefaultLandsInSandbox_ControlHomeReachesRealStore
// reproduces the sybra#1576 wipe with the sybra#1577 fix in place: a
// freshly-built sybra-cli, launched as a real subprocess through
// agent.Manager.Run (the actual dispatch chokepoint, not a mocked config),
// must (a) resolve its own default SYBRA_HOME to the per-task sandbox rather
// than any real Sybra home, and (b) still reach the real operator task store
// through SYBRA_CONTROL_HOME for its own task's `sybra-cli update` call.
func TestSybraHomeSentinel_DefaultLandsInSandbox_ControlHomeReachesRealStore(t *testing.T) {
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "sybra_home_sentinel")

	// The "real" operator store — never sees SYBRA_HOME directly, only
	// SYBRA_CONTROL_HOME injected by the manager.
	realHome, err := os.MkdirTemp("", "sybra-e2e-realhome-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(realHome) })
	realTasksDir := filepath.Join(realHome, "tasks")
	if err := os.MkdirAll(realTasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realStore, err := task.NewStore(realTasksDir)
	if err != nil {
		t.Fatal(err)
	}
	realTasks := task.NewManager(realStore, nil)
	tk, err := realTasks.Create("sentinel task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// The per-task sandbox home — must be what the agent subprocess sees as
	// its default SYBRA_HOME.
	sandboxHome, err := os.MkdirTemp("", "sybra-e2e-sandboxhome-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sandboxHome) })

	// A bogus ambient SYBRA_HOME/SYBRA_TASKS_DIR simulating whatever the
	// Sybra app process itself happens to be running under — must never leak
	// into the agent subprocess or its sybra-cli calls.
	t.Setenv("SYBRA_HOME", filepath.Join(t.TempDir(), "ambient-should-not-be-used"))
	t.Setenv("SYBRA_TASKS_DIR", "")

	logger := e2eLogger(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	logDir, err := os.MkdirTemp("", "sybra-e2e-logs-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { keepAgentLogsOnFailure(t, logDir) })

	agentMgr := newTestAgentManager(t, ctx, func(string, any) {}, logger, logDir, agent.ManagerConfig{
		Runtime:     agent.ManagerRuntimeConfig{DefaultProvider: "claude"},
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
		ControlHome: realHome,
	})

	dir := t.TempDir()
	a, err := agentMgr.Run(agent.RunConfig{
		TaskID: tk.ID,
		Name:   tk.Title,
		Mode:   "headless",
		Prompt: "Task " + tk.ID + ": run the sentinel",
		Dir:    dir,
	})
	if err != nil {
		t.Fatalf("agentMgr.Run: %v", err)
	}

	waitFor(t, defaultE2ETimeout, "sentinel agent to finish", func() bool {
		return a.GetState() != agent.StateRunning
	})

	// (a) Default SYBRA_HOME landed in the sandbox, not the real home or the
	// bogus ambient one.
	waitFor(t, defaultE2ETimeout, "sentinel marker written under the sandbox home", func() bool {
		_, statErr := os.Stat(filepath.Join(sandboxHome, "sentinel-marker"))
		return statErr == nil
	})

	// (b) Bare `sybra-cli update` (no --home) reached the real operator store
	// via SYBRA_CONTROL_HOME.
	waitFor(t, defaultE2ETimeout, "real task flips to in-review via SYBRA_CONTROL_HOME", func() bool {
		got, getErr := realTasks.Get(tk.ID)
		return getErr == nil && got.Status == task.StatusInReview
	})
}
