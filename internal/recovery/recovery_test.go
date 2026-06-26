package recovery_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// fakeGate is a provider.HealthGate stub. IsHealthy/RateLimited return the
// configured flags so tests can simulate a rate-limited (or auth-failed)
// provider.
type fakeGate struct {
	healthy     bool
	rateLimited bool
}

func (f fakeGate) IsHealthy(string) bool   { return f.healthy }
func (f fakeGate) RateLimited(string) bool { return f.rateLimited }
func (f fakeGate) Failover(string) string  { return "" }
func (f fakeGate) Reason(string) string {
	if f.rateLimited {
		return "rate_limited"
	}
	return ""
}
func (f fakeGate) ReportAuthFailure(string, string)              {}
func (f fakeGate) ReportRateLimit(string, time.Duration, string) {}

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

// TestRestartStaleSkipsRateLimitedProvider verifies a stalled task whose last
// run's provider is rate-limited is left in-progress (not re-dispatched, which
// would just hit the limit again). The debounce is satisfied (old run) so the
// provider gate is the only thing that can hold it back.
func TestRestartStaleSkipsRateLimitedProvider(t *testing.T) {
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
	agents.SetHealthGate(fakeGate{rateLimited: true}) // provider rate-limited
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	created, err := tasks.Create("rate-limited", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/repo"
	if _, err := tasks.Update(created.ID, task.Update{Status: &inProg, ProjectID: &projID}); err != nil {
		t.Fatal(err)
	}
	// Old run → debounce passes, so only the provider gate can stop re-dispatch.
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-1",
		Mode:      "headless",
		Provider:  "claude",
		State:     string(agent.StateStopped),
		StartedAt: time.Now().Add(-30 * time.Minute),
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
		t.Errorf("orchestrator was called %d times; want 0 (provider rate-limited)", stub.startCalls)
	}
}

// TestRestartStaleSteerBypassesRecentRunDebounce verifies the recovery half of
// a watchdog headless nudge: a pending SupervisorSteer makes a just-stopped task
// re-dispatch immediately instead of waiting out the recent-run debounce. The
// steer is consumed + prepended inside AgentOrchestrator.StartAgent (covered by
// the internal/sybra helper test), not here.
func TestRestartStaleSteerBypassesRecentRunDebounce(t *testing.T) {
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

	created, err := tasks.Create("looping", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/repo"
	steer := "stop retrying the failing command; read the error first"
	if _, err := tasks.Update(created.ID, task.Update{
		Status:          &inProg,
		ProjectID:       &projID,
		SupervisorSteer: &steer,
	}); err != nil {
		t.Fatal(err)
	}
	// RECENT run: the debounce would normally skip it, but a pending steer
	// (the watchdog just stopped a looping agent) must bypass the debounce.
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-1",
		Mode:      "headless",
		State:     string(agent.StateStopped),
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
		Projects:     stubProjects{},
		Logger:       logger,
		Throttle:     logging.NewErrorThrottle(),
		WG:           &wg,
		LogDir:       t.TempDir(),
	}
	r.RestartStaleInProgress()
	wg.Wait()

	if stub.startCalls != 1 {
		t.Fatalf("dispatch count = %d, want 1 (steer must bypass the recent-run debounce)", stub.startCalls)
	}
}

type stubOrchestrator struct {
	startCalls    int
	prFixCalls    int
	startErr      error
	prFixErr      error
	startReturned *agent.Agent
	lastPrompt    string
}

func (s *stubOrchestrator) StartAgent(_, _, prompt string, _, _ bool) (*agent.Agent, error) {
	s.startCalls++
	s.lastPrompt = prompt
	return s.startReturned, s.startErr
}

func (s *stubOrchestrator) StartPRFixAgent(_ string) error {
	s.prFixCalls++
	return s.prFixErr
}

// stubProjects is a recovery.ProjectGetter that reports no project so the
// re-dispatch prompt uses the default (non-pet) PR flag.
type stubProjects struct{}

func (stubProjects) Get(string) (project.Project, error) {
	return project.Project{}, errors.New("no project")
}
