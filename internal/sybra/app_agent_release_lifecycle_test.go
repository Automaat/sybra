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

// newFakeSteerableClaudeApp builds a real *App (unmocked taskAgentReleaser)
// backed by a real agent.Manager with restart-survival enabled, and installs
// a fake `claude` binary on PATH that blocks reading its never-EOF FIFO
// stdin — the same shape a live, detached steerable-headless session
// mid-turn has in production.
func newFakeSteerableClaudeApp(t *testing.T) *App {
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
		Runtime:           agent.ManagerRuntimeConfig{DefaultProvider: "claude", HeadlessSteerable: true},
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

// startIdleSteerableAgent runs a detached steerable-headless "claude"
// session for taskID and waits for its subprocess to come up and block on
// its never-EOF FIFO stdin (mid-turn, awaiting the next turn boundary — a
// live process holding an agent.max_concurrent slot). Returns the agent and
// its OS pid.
func startIdleSteerableAgent(t *testing.T, a *App, taskID string) (ag *agent.Agent, pid int) {
	t.Helper()
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:            taskID,
		Name:              "implementation",
		Mode:              "headless",
		HeadlessSteerable: true,
		Provider:          "claude",
		Dir:               t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && ag.GetPID() <= 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if st := ag.GetState(); st != agent.StateRunning {
		t.Fatalf("agent did not settle into running state, got %s", st)
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

// TestApp_TaskTerminal_AsksLiveSteerableAgentToExit is the regression test
// for #2290: a live agent process has no lifecycle tie-back to its owning
// task, so it survives task completion indefinitely, permanently holding an
// agent.max_concurrent slot. It reproduces the exact production shape — a
// detached (restart-surviving) steerable-headless claude session blocked
// mid-turn on a never-EOF stdin FIFO — and asserts that moving the owning
// task to a terminal status asks the real OS process to exit within a
// bounded time.
func TestApp_TaskTerminal_AsksLiveSteerableAgentToExit(t *testing.T) {
	for _, target := range []task.Status{task.StatusDone, task.StatusCancelled} {
		t.Run(string(target), func(t *testing.T) {
			a := newFakeSteerableClaudeApp(t)

			created, err := a.tasks.Create("steerable session outlives task", "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			_, pid := startIdleSteerableAgent(t, a, created.ID)

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
			t.Fatalf("agent process (pid %d) still alive 5s after task reached %s", pid, target)
		})
	}
}

// releaseTaskAgents spares agents started after a human-required park, so the
// park cannot reap the very agent dispatched to clear it. That exemption must
// not extend to terminal statuses: a done task has no more work, so an agent
// running against it is a leak regardless of when it started.
func TestApp_ReleaseTaskAgents_TerminalReapsEvenNewerAgent(t *testing.T) {
	a := newFakeSteerableClaudeApp(t)

	created, err := a.tasks.Create("terminal reap", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.tasks.Apply(task.TransitionIntent{
		TaskID: created.ID, ToStatus: task.StatusDone, Actor: "test", OperatorOverride: true,
	}); err != nil {
		t.Fatalf("mark done: %v", err)
	}

	// This agent starts strictly after the terminal transition, which is the
	// exact shape the human-required exemption spares.
	ag, pid := startIdleSteerableAgent(t, a, created.ID)

	a.releaseTaskAgents(created.ID)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && processAliveForTest(pid) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAliveForTest(pid) {
		t.Fatalf("agent %s still running against a done task; the newer-agent exemption must not apply to terminal statuses", ag.ID)
	}
}
