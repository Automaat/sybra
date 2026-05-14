package recovery_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestRunStartupCleanupEmpty verifies the boot pass is idempotent on a
// fresh, empty store — no panics, no error returns, no spurious task
// mutations.
func TestRunStartupCleanupEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	logger := discardLogger()
	agents := agent.NewManager(t.Context(), func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	var wg sync.WaitGroup
	r := &recovery.Recovery{
		Tasks:     tasks,
		Agents:    agents,
		Worktrees: wm,
		Logger:    logger,
		Throttle:  logging.NewErrorThrottle(),
		WG:        &wg,
		LogDir:    t.TempDir(),
	}
	r.RunStartupCleanup()
	wg.Wait()
}

// TestRestartStaleSkipsRecentRun verifies the dev-reload debounce: an
// in-progress task whose latest agent run started seconds ago is left
// alone (no respawn) so a hot-reloaded App doesn't race a still-living
// subprocess from the prior lifecycle.
func TestRestartStaleSkipsRecentRun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := discardLogger()
	agents := agent.NewManager(ctx, func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	created, err := tasks.Create("stuck", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/repo"
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    &inProg,
		ProjectID: &projID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-1",
		Mode:      "headless",
		State:     string(agent.StateRunning),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stub := stubOrchestrator{}
	r := &recovery.Recovery{
		Tasks:        tasks,
		Agents:       agents,
		Worktrees:    wm,
		Orchestrator: &stub,
		Logger:       logger,
		Throttle:     logging.NewErrorThrottle(),
		WG:           &wg,
		LogDir:       t.TempDir(),
	}
	r.RestartStaleInProgress()
	wg.Wait()

	if stub.startCalls != 0 {
		t.Errorf("orchestrator was called %d times; want 0 (recent run debounce)", stub.startCalls)
	}
}

type stubOrchestrator struct {
	startCalls    int
	prFixCalls    int
	startErr      error
	prFixErr      error
	startReturned *agent.Agent
}

func (s *stubOrchestrator) StartAgent(_, _, _ string, _, _ bool) (*agent.Agent, error) {
	s.startCalls++
	return s.startReturned, s.startErr
}

func (s *stubOrchestrator) StartPRFixAgent(_ string) error {
	s.prFixCalls++
	return s.prFixErr
}
