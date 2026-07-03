package recovery_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
	"github.com/Automaat/sybra/internal/worktreeerr"
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
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
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
	r.RunStartupCleanup(context.Background())
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
	agents := newTestAgentManager(t, ctx, func(string, any) {}, logger, t.TempDir())
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
	r.RestartStaleInProgress(context.Background())
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
	agents := newTestAgentManager(t, ctx, func(string, any) {}, logger, t.TempDir())
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
	r.RestartStaleInProgress(context.Background())
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
	agents := newTestAgentManager(t, ctx, func(string, any) {}, logger, t.TempDir())
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
	r.RestartStaleInProgress(context.Background())
	wg.Wait()

	if stub.startCalls != 1 {
		t.Fatalf("dispatch count = %d, want 1 (steer must bypass the recent-run debounce)", stub.startCalls)
	}
}

func TestRestartStaleInteractiveOneShotRestartsAsOneShot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := discardLogger()
	agents := newTestAgentManager(t, ctx, func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	created, err := tasks.Create("interactive one-shot", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/repo"
	if _, err := tasks.Update(created.ID, task.Update{Status: &inProg, ProjectID: &projID}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-one-shot",
		Mode:      "interactive",
		State:     string(agent.StateStopped),
		StartedAt: time.Now().Add(-30 * time.Minute),
		OneShot:   true,
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
	r.RestartStaleInProgress(context.Background())
	wg.Wait()

	if stub.startCalls != 1 {
		t.Fatalf("dispatch count = %d, want 1", stub.startCalls)
	}
	if stub.lastMode != "interactive" {
		t.Fatalf("mode = %q, want interactive", stub.lastMode)
	}
	if !stub.lastOneShot {
		t.Fatal("interactive one-shot stale restart must preserve oneShot=true")
	}
}

// TestRestartStalePRFixRebaseFailedFlipsToHumanRequired covers the actual
// path that had the infinite-retry bug this PR fixes: run_role=="pr-fix"
// skips markRebaseBlocked (unlike the other three agent-start call sites) and
// returns worktreeerr.ErrRebaseFailed straight through to
// Recovery.surfaceStartFailure. Before ClassifyAgentStartError learned to
// treat it as permanent, RestartStaleInProgress would keep re-dispatching
// StartPRFixAgent against the same doomed rebase every restartStaleMinAge
// tick, forever.
func TestRestartStalePRFixRebaseFailedFlipsToHumanRequired(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := discardLogger()
	agents := newTestAgentManager(t, ctx, func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	created, err := tasks.Create("pr-fix rebase failed", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/repo"
	runRole := "pr-fix"
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    &inProg,
		ProjectID: &projID,
		RunRole:   &runRole,
	}); err != nil {
		t.Fatal(err)
	}
	// Old run → debounce passes so the pr-fix branch actually re-dispatches.
	// Role is deliberately NOT "pr-fix" here: a last run with Role=="pr-fix"
	// hits the earlier revert-to-in-review shortcut (stale.go) before this
	// dispatch is ever reached — task.RunRole (not AgentRun.Role) is what
	// selects the StartPRFixAgent branch under test.
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-impl",
		Mode:      "headless",
		State:     string(agent.StateStopped),
		StartedAt: time.Now().Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stub := stubOrchestrator{prFixErr: fmt.Errorf("prepare worktree: %w", worktreeerr.ErrRebaseFailed)}
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
	r.RestartStaleInProgress(context.Background())
	wg.Wait()

	if stub.prFixCalls != 1 {
		t.Fatalf("StartPRFixAgent calls = %d, want 1", stub.prFixCalls)
	}

	updated, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusHumanRequired {
		t.Errorf("status = %s, want %s (rebase failure must escalate, not retry forever)", updated.Status, task.StatusHumanRequired)
	}
	if !strings.Contains(updated.StatusReason, "branch stale") {
		t.Errorf("status reason = %q, want rebase-failed classification", updated.StatusReason)
	}
}

// stubWorkflowEngine implements recovery.WorkflowRestarter for tests that need
// a non-nil engine without a real workflow store.
type stubWorkflowEngine struct {
	startWorkflowCalls int
}

func (s *stubWorkflowEngine) StartWorkflow(_, _ string) error {
	s.startWorkflowCalls++
	return nil
}

func (s *stubWorkflowEngine) HandleAgentComplete(_ string, _ workflow.AgentCompletion) {}

// TestRestartStalePRFixWorkflowRevertsToInReview verifies the root cause of the
// b319d12f incident: a task left in-progress with a cancelled (ExecCompleted)
// pr-fix workflow must NOT have StartWorkflow called on restart — that would
// launch the agent with nil vars (wrong dir, empty prompt). Instead the task
// reverts to in-review so the PR monitor re-detects and re-dispatches.
func TestRestartStalePRFixWorkflowRevertsToInReview(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := discardLogger()
	agents := newTestAgentManager(t, ctx, func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	// Simulate the post-cancel shape: in-progress task with an ExecCompleted
	// pr-fix workflow (what cancelResolvedPRFixWorkflows leaves behind).

	created, err := tasks.Create("cancelled pr-fix", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	status := task.StatusInProgress
	wf := &workflow.Execution{
		WorkflowID:  "pr-fix",
		State:       workflow.ExecCompleted,
		Variables:   map[string]string{"cancel_reason": "pr-monitor: conflict resolved"},
		CompletedAt: task.Ptr(time.Now().UTC()),
	}
	if _, err := tasks.Update(created.ID, task.Update{Status: &status, Workflow: &wf}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-pr-fix",
		Role:      "pr-fix",
		Mode:      "headless",
		State:     string(agent.StateStopped),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stub := stubOrchestrator{}
	wfStub := &stubWorkflowEngine{}
	r := &recovery.Recovery{
		Tasks:          tasks,
		Agents:         agents,
		Worktrees:      wm,
		Orchestrator:   &stub,
		WorkflowEngine: wfStub,
		Logger:         logger,
		Throttle:       logging.NewErrorThrottle(),
		WG:             &wg,
		LogDir:         t.TempDir(),
	}
	r.RestartStaleInProgress(context.Background())
	wg.Wait()

	if stub.startCalls != 0 || stub.prFixCalls != 0 {
		t.Errorf("orchestrator called (start=%d prFix=%d); want 0", stub.startCalls, stub.prFixCalls)
	}
	if wfStub.startWorkflowCalls != 0 {
		t.Errorf("WorkflowEngine.StartWorkflow called %d times; want 0", wfStub.startWorkflowCalls)
	}

	updated, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusInReview {
		t.Errorf("task status = %s; want %s", updated.Status, task.StatusInReview)
	}
	if !strings.Contains(updated.StatusReason, "conflict resolved") {
		t.Errorf("status reason = %q, want cancellation reason", updated.StatusReason)
	}
}

type stubOrchestrator struct {
	startCalls    int
	prFixCalls    int
	startErr      error
	prFixErr      error
	startReturned *agent.Agent
	lastMode      string
	lastPrompt    string
	lastOneShot   bool
}

func (s *stubOrchestrator) StartAgent(_, mode, prompt string, _, oneShot bool) (*agent.Agent, error) {
	s.startCalls++
	s.lastMode = mode
	s.lastPrompt = prompt
	s.lastOneShot = oneShot
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
