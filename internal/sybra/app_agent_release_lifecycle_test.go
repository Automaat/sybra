package sybra

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// newFakeInteractiveClaudeApp builds a real *App (unmocked taskAgentReleaser)
// backed by a real agent.Manager with restart-survival enabled, and installs
// a fake `claude` binary on PATH that blocks reading its never-EOF FIFO
// stdin — the same shape a live, idle (StatePaused) interactive session has
// in production.
func newFakeInteractiveClaudeApp(t *testing.T) *App {
	t.Helper()

	fakebin := t.TempDir()
	fakeClaude := filepath.Join(fakebin, "claude")
	// exec replaces the shell with `cat`, which blocks reading the detached
	// FIFO stdin forever — the "waiting for a turn that never arrives" shape
	// from #2290.
	if err := os.WriteFile(fakeClaude, []byte("#!/usr/bin/env bash\nexec cat >/dev/null\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir, err := os.MkdirTemp("", "sybra-test-tasks-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	logger := discardLogger()
	mgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir(), agent.ManagerConfig{
		Runtime:           agent.ManagerRuntimeConfig{DefaultProvider: "claude"},
		SurviveRestartDir: t.TempDir(),
		SandboxHome:       func(string) (string, error) { return t.TempDir(), nil },
	})
	t.Cleanup(func() { mgr.ShutdownWithGrace(2 * time.Second) })

	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        taskMgr,
		Logger:       logger,
		AgentChecker: mgr.HasRunningAgentForTask,
	})
	agentOrch := agentorch.New(taskMgr, nil, mgr, nil, logger, wm, nil)

	a := &App{
		tasks:     taskMgr,
		agents:    mgr,
		tasksDir:  dir,
		logger:    logger,
		worktrees: wm,
		agentOrch: agentOrch,
	}
	a.initStatusHook()
	return a
}

// startIdleInteractiveAgent runs a detached interactive "claude" session for
// taskID and waits for it to settle into StatePaused (idle, awaiting the
// next turn — a live process holding an agent.max_concurrent slot). Returns
// the agent and its OS pid.
func startIdleInteractiveAgent(t *testing.T, a *App, taskID string) (ag *agent.Agent, pid int) {
	t.Helper()
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:   taskID,
		Name:     "implementation",
		Mode:     "interactive",
		Provider: "claude",
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && ag.GetState() != agent.StatePaused {
		time.Sleep(10 * time.Millisecond)
	}
	if ag.GetState() != agent.StatePaused {
		t.Fatalf("agent did not settle into paused state, got %s", ag.GetState())
	}
	pid = ag.GetPID()
	if pid <= 0 {
		t.Fatalf("agent has no pid")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("fake claude process not alive right after start: %v", err)
	}
	// Detached agents are exempt from Manager.Shutdown's own cleanup by
	// design (restart survival), so a failed assertion before the release
	// path runs would otherwise leave this process orphaned. Guard on
	// liveness first — the pid may already be gone (and reused by an
	// unrelated process) by the time cleanup runs on a passing test.
	t.Cleanup(func() {
		if processAliveForTest(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return ag, pid
}

func processAliveForTest(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestApp_TaskTerminal_AsksLiveInteractiveAgentToExit is the regression test
// for #2290: an interactive-mode agent process has no lifecycle tie-back to
// its owning task, so it survives task completion indefinitely, permanently
// holding an agent.max_concurrent slot. It reproduces the exact production
// shape — a detached (restart-surviving) interactive claude session idling
// on a never-EOF stdin FIFO — and asserts that moving the owning task to a
// terminal status asks the real OS process to exit within a bounded time.
func TestApp_TaskTerminal_AsksLiveInteractiveAgentToExit(t *testing.T) {
	for _, target := range []task.Status{task.StatusDone, task.StatusCancelled} {
		t.Run(string(target), func(t *testing.T) {
			a := newFakeInteractiveClaudeApp(t)

			created, err := a.tasks.Create("interactive session outlives task", "", "interactive")
			if err != nil {
				t.Fatal(err)
			}
			_, pid := startIdleInteractiveAgent(t, a, created.ID)

			if _, err := a.tasks.Update(created.ID, task.Update{Status: task.Ptr(target)}); err != nil {
				t.Fatal(err)
			}

			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if !processAliveForTest(pid) {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("interactive agent process (pid %d) still alive 5s after task reached %s", pid, target)
		})
	}
}
