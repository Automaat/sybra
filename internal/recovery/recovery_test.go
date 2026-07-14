package recovery_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/recovery"
	"github.com/Automaat/sybra/internal/sandbox"
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

func TestRunStartupCleanup_CleansOrphanedSandboxes(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	active, err := tasks.Create("active sandbox task", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	done, err := tasks.Create("done sandbox task", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	doneStatus := task.StatusDone
	done, err = tasks.Update(done.ID, task.Update{Status: &doneStatus})
	if err != nil {
		t.Fatal(err)
	}

	sbDir := t.TempDir()
	sbMgr := sandbox.NewManager(sbDir, discardLogger())
	activeHome, err := sbMgr.SybraHomeDir(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	doneHome, err := sbMgr.SybraHomeDir(done.ID)
	if err != nil {
		t.Fatal(err)
	}
	missingRoot := filepath.Join(sbDir, "missing-task")
	if err := os.MkdirAll(missingRoot, 0o755); err != nil {
		t.Fatal(err)
	}

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
		Sandboxes: sbMgr,
		Logger:    logger,
		Throttle:  logging.NewErrorThrottle(),
		WG:        &wg,
		LogDir:    t.TempDir(),
	}
	r.RunStartupCleanup(context.Background())
	wg.Wait()

	if _, err := os.Stat(filepath.Dir(activeHome)); err != nil {
		t.Fatalf("active sandbox dir removed unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(doneHome)); !os.IsNotExist(err) {
		t.Fatalf("done sandbox dir still exists after startup cleanup: %v", err)
	}
	if _, err := os.Stat(missingRoot); !os.IsNotExist(err) {
		t.Fatalf("missing sandbox dir still exists after startup cleanup: %v", err)
	}
}

// TestRunStartupCleanup_GcOrphanChatIsTrashedNotLost verifies the
// gcOrphanChats → pruneTrash ordering: an orphaned chat task gc'd during
// startup goes through Tasks.Delete (now a soft delete, see
// internal/task.Store.Delete), so a wrongly-collected chat is still
// recoverable via trash restore rather than gone for good. pruneTrash runs
// immediately after gcOrphanChats in the same pass, but with retention
// unset (0 → default 14 days) a same-day trash entry must survive it.
func TestRunStartupCleanup_GcOrphanChatIsTrashedNotLost(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	chat, err := tasks.CreateChat("owner/repo")
	if err != nil {
		t.Fatal(err)
	}

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

	if _, err := tasks.Get(chat.ID); err == nil {
		t.Fatal("orphaned chat task should no longer be live after startup cleanup")
	}

	entries, err := tasks.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != chat.ID {
		t.Fatalf("ListTrash() = %+v, want the gc'd chat task preserved in trash", entries)
	}
}

func TestRunStartupCleanup_ReapsDeletedCWDOrphanProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only process enumeration test")
	}

	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	logger := discardLogger()
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	worktreesDir := t.TempDir()
	wm := worktree.New(worktree.Config{
		WorktreesDir: worktreesDir,
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	orphanCWD := filepath.Join(worktreesDir, "orphan-task")
	if err := os.MkdirAll(orphanCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	proc := exec.Command(sleepBin, "30")
	proc.Dir = orphanCWD
	if err := proc.Start(); err != nil {
		t.Fatalf("start orphan process: %v", err)
	}
	t.Cleanup(func() { _ = proc.Process.Kill() })
	go func() { _ = proc.Wait() }()

	if err := os.RemoveAll(orphanCWD); err != nil {
		t.Fatalf("remove orphan cwd: %v", err)
	}
	waitForDeletedRecoveryCWD(t, proc.Process.Pid)

	var wg sync.WaitGroup
	r := &recovery.Recovery{
		Tasks:       tasks,
		Agents:      agents,
		Worktrees:   wm,
		Logger:      logger,
		Throttle:    logging.NewErrorThrottle(),
		WG:          &wg,
		LogDir:      t.TempDir(),
		OrphanRoots: []string{worktreesDir},
	}
	r.RunStartupCleanup(context.Background())
	wg.Wait()
	waitForRecoveryProcessExit(t, proc.Process.Pid)
}

// TestRunStartupCleanup_PruneTrashRemovesExpiredGenerations verifies
// pruneTrash actually runs during RunStartupCleanup and respects
// TrashRetentionDays: an old, already-expired trash generation is gone
// after the pass.
func TestRunStartupCleanup_PruneTrashRemovesExpiredGenerations(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	created, err := tasks.Create("Expired", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := tasks.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListTrash() = %d entries, want 1", len(entries))
	}
	backdated := filepath.Join(store.TrashDir(), time.Now().UTC().AddDate(0, 0, -30).Format(time.DateOnly))
	if err := os.Rename(filepath.Join(store.TrashDir(), entries[0].DeletedDate), backdated); err != nil {
		t.Fatal(err)
	}

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
		Tasks:              tasks,
		Agents:             agents,
		Worktrees:          wm,
		Logger:             logger,
		Throttle:           logging.NewErrorThrottle(),
		WG:                 &wg,
		LogDir:             t.TempDir(),
		TrashRetentionDays: 14,
	}
	r.RunStartupCleanup(context.Background())
	wg.Wait()

	remaining, err := tasks.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %+v, want the expired generation pruned", remaining)
	}
}

func TestRunStartupCleanup_PruneTrashZeroUsesDefaultRetention(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	created, err := tasks.Create("Expired", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := tasks.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListTrash() = %d entries, want 1", len(entries))
	}
	backdated := filepath.Join(store.TrashDir(), time.Now().UTC().AddDate(0, 0, -30).Format(time.DateOnly))
	if err := os.Rename(filepath.Join(store.TrashDir(), entries[0].DeletedDate), backdated); err != nil {
		t.Fatal(err)
	}

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
		Tasks:              tasks,
		Agents:             agents,
		Worktrees:          wm,
		Logger:             logger,
		Throttle:           logging.NewErrorThrottle(),
		WG:                 &wg,
		LogDir:             t.TempDir(),
		TrashRetentionDays: 0,
	}
	r.RunStartupCleanup(context.Background())
	wg.Wait()

	remaining, err := tasks.ListTrash()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %+v, want the default 14-day retention to prune the expired generation", remaining)
	}
}

// TestRestartStaleSkipsRecentRun verifies the dev-reload debounce: an
// in-progress task whose latest agent run started seconds ago is left
// alone (no respawn) so a hot-reloaded App doesn't race a still-living
// subprocess from the prior lifecycle.
func TestRestartStaleSkipsGatedTask(t *testing.T) {
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

	created, err := tasks.Create("remote", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/pet"
	if _, err := tasks.Update(created.ID, task.Update{Status: &inProg, ProjectID: &projID}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-1",
		Mode:      "headless",
		State:     string(agent.StateRunning),
		StartedAt: time.Now().Add(-time.Hour),
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
		DispatchGate: func(tk task.Task) bool { return tk.ProjectID != "owner/pet" },
	}
	r.RestartStaleInProgress(context.Background())
	wg.Wait()

	if stub.startCalls != 0 {
		t.Errorf("gated (remote-homed) task was restarted %d times; want 0 — the leader must not run a follower's task", stub.startCalls)
	}
}

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

func TestRestartStaleSkipsWhileDispatchInFlight(t *testing.T) {
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

	created, err := tasks.Create("dispatching", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/repo"
	if _, err := tasks.Update(created.ID, task.Update{Status: &inProg, ProjectID: &projID}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-1",
		Mode:      "headless",
		Provider:  "claude",
		State:     string(agent.StateStopped),
		StartedAt: time.Now().Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	claim, ok := agents.TryClaimDispatch(created.ID)
	if !ok {
		t.Fatal("expected to acquire dispatch claim")
	}
	defer claim.Release()

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
		t.Errorf("orchestrator was called %d times; want 0 (dispatch in flight)", stub.startCalls)
	}
}

// TestRestartStaleSteerBypassesRecentRunDebounce verifies the recovery half of
// a watchdog headless nudge: a pending SupervisorSteer makes a just-stopped task
// re-dispatch immediately instead of waiting out the recent-run debounce. The
// steer is consumed + prepended inside agentorch.Orchestrator.StartAgent (covered by
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

func TestRestartStaleInteractiveNoRunRedispatchesWhenProjectAssigned(t *testing.T) {
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

	created, err := tasks.Create("zero-run stale", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/repo"
	if _, err := tasks.Update(created.ID, task.Update{Status: &inProg, ProjectID: &projID}); err != nil {
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
	if stub.lastOneShot {
		t.Fatal("zero-run stale restart must not force oneShot")
	}
}

// TestRestartStaleInteractiveModeMismatchRedispatches covers the Copilot
// review finding on this PR: a task whose AgentMode was flipped to
// interactive (e.g. by selfmonitor's flipAgentMode) after its last recorded
// run was headless must not be silently swallowed by
// recoverStaleInteractive's mode no-op — it should fall through to the
// normal restart path instead of getting stuck in-progress forever.
func TestRestartStaleInteractiveModeMismatchRedispatches(t *testing.T) {
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

	created, err := tasks.Create("mode-mismatch stale", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	projID := "owner/repo"
	if _, err := tasks.Update(created.ID, task.Update{Status: &inProg, ProjectID: &projID}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "ag-headless-stale",
		Mode:      "headless",
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
	if stub.lastOneShot {
		t.Fatal("mode-mismatch stale restart must not force oneShot")
	}
}

func TestRestartStaleInteractiveNoRunWithoutProjectEscalates(t *testing.T) {
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

	created, err := tasks.Create("zero-run stale missing project", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	inProg := task.StatusInProgress
	if _, err := tasks.Update(created.ID, task.Update{Status: &inProg}); err != nil {
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
		t.Fatalf("dispatch count = %d, want 0", stub.startCalls)
	}
	updated, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusHumanRequired {
		t.Fatalf("status = %s, want %s", updated.Status, task.StatusHumanRequired)
	}
	if !strings.Contains(updated.StatusReason, "no project could be assigned") {
		t.Fatalf("status reason = %q, want no-project assignment guidance", updated.StatusReason)
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
	completions        []workflow.AgentCompletion

	dispatchEventCalls  []map[string]string
	dispatchEventResult string
	dispatchEventErr    error
}

func (s *stubWorkflowEngine) StartWorkflow(_, _ string) error {
	s.startWorkflowCalls++
	return nil
}

func (s *stubWorkflowEngine) DispatchEvent(_, _ string, extraFields, _ map[string]string) (string, error) {
	s.dispatchEventCalls = append(s.dispatchEventCalls, extraFields)
	return s.dispatchEventResult, s.dispatchEventErr
}

func (s *stubWorkflowEngine) HandleAgentComplete(_ string, c workflow.AgentCompletion) {
	s.completions = append(s.completions, c)
}

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

// newTerminalWorkflowInProgressTask creates an in-progress task whose
// recorded workflow (wfID) already reached a terminal state — the shape left
// behind after, e.g., a testing bounce escalates to human-required and an
// out-of-band caller (a monitor auto-retry, an operator dispatch) then flips
// status back to in-progress without touching the stale Workflow field.
func newTerminalWorkflowInProgressTask(t *testing.T, tasks *task.Manager, wfID string) string {
	t.Helper()
	created, err := tasks.Create("resume after testing bounce", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	status := task.StatusInProgress
	wf := &workflow.Execution{
		WorkflowID:  wfID,
		State:       workflow.ExecCompleted,
		CompletedAt: task.Ptr(time.Now().UTC()),
	}
	if _, err := tasks.Update(created.ID, task.Update{Status: &status, Workflow: &wf}); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// TestRestartStaleTerminalWorkflowRedispatchesByCurrentStatus verifies the
// fix for #1657: a task auto-resumed from human-required back to in-progress
// still carries whatever workflow last ran (here: testing-task, from a
// testing bounce) with a terminal state. Blindly restarting that stale
// WorkflowID would replay testing (or, for a task that escalated out of
// planning, triage/plan) instead of resuming implementation. Recovery must
// redispatch via task.status_changed so the trigger system lands on
// simple-task-implement, and must NOT fall back to the stale StartWorkflow
// call when DispatchEvent already found a match.
func TestRestartStaleTerminalWorkflowRedispatchesByCurrentStatus(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := discardLogger()
	agents := newTestAgentManager(t, context.Background(), func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	taskID := newTerminalWorkflowInProgressTask(t, tasks, "testing-task")

	var wg sync.WaitGroup
	stub := stubOrchestrator{}
	wfStub := &stubWorkflowEngine{dispatchEventResult: "simple-task-implement"}
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

	if len(wfStub.dispatchEventCalls) != 1 {
		t.Fatalf("DispatchEvent called %d times; want 1", len(wfStub.dispatchEventCalls))
	}
	if got := wfStub.dispatchEventCalls[0]["task.status"]; got != string(task.StatusInProgress) {
		t.Errorf("DispatchEvent task.status = %q; want %q", got, task.StatusInProgress)
	}
	if wfStub.startWorkflowCalls != 0 {
		t.Errorf("StartWorkflow called %d times; want 0 (DispatchEvent already matched)", wfStub.startWorkflowCalls)
	}

	updated, err := tasks.Get(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusInProgress {
		t.Errorf("task status = %s; want %s (unchanged, dispatch owns any further transition)", updated.Status, task.StatusInProgress)
	}
}

// TestRestartStaleTerminalWorkflowFallsBackWhenNoTriggerMatches verifies the
// safety net: if DispatchEvent finds no builtin trigger matching the task's
// current status, recovery still falls back to restarting the recorded
// WorkflowID directly rather than silently leaving the task inert.
func TestRestartStaleTerminalWorkflowFallsBackWhenNoTriggerMatches(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := discardLogger()
	agents := newTestAgentManager(t, context.Background(), func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	newTerminalWorkflowInProgressTask(t, tasks, "testing-task")

	var wg sync.WaitGroup
	stub := stubOrchestrator{}
	wfStub := &stubWorkflowEngine{} // dispatchEventResult == "" -> no match
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

	if len(wfStub.dispatchEventCalls) != 1 {
		t.Fatalf("DispatchEvent called %d times; want 1", len(wfStub.dispatchEventCalls))
	}
	if wfStub.startWorkflowCalls != 1 {
		t.Errorf("StartWorkflow called %d times; want 1 (fallback)", wfStub.startWorkflowCalls)
	}
}

// newReviewTaskWithHeadlessRun creates an in-progress, review-tagged task
// with a non-terminal workflow parked mid-step and a single headless run
// whose lifecycle fields are the caller's to fill in. Shared setup for the
// recoverCompletedHeadlessRun outcome-gating tests below.
func newReviewTaskWithHeadlessRun(t *testing.T, tasks *task.Manager, run task.AgentRun) string {
	t.Helper()
	created, err := tasks.Create("review me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	status := task.StatusInProgress
	tags := []string{"review"}
	wf := &workflow.Execution{
		WorkflowID:  "pr-review",
		State:       workflow.ExecRunning,
		CurrentStep: "review_simple",
		StartedAt:   time.Now(),
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:   &status,
		Tags:     &tags,
		Workflow: &wf,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, run); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// TestRestartStaleRecoverCompletedHeadlessRunKeysOnOutcome verifies recovery
// replays a lost headless completion using the persisted AgentRun.Outcome
// rather than inferring success from State ("stopped" covers both a clean
// finish and a failed one) or Result's presence (truncated, so non-empty
// proves nothing). This is the regression covered by #1537: a failed review
// run lost across a restart must not be replayed as a success.
func TestRestartStaleRecoverCompletedHeadlessRunKeysOnOutcome(t *testing.T) {
	cases := []struct {
		name        string
		outcome     string
		result      string
		wantHandled bool
		wantSuccess bool
	}{
		{
			name:        "success outcome replays success",
			outcome:     task.RunOutcomeSuccess,
			result:      "review posted",
			wantHandled: true,
			wantSuccess: true,
		},
		{
			name:        "failure outcome replays failure, not success",
			outcome:     task.RunOutcomeFailure,
			result:      "review posted before crash",
			wantHandled: true,
			wantSuccess: false,
		},
		{
			name:        "missing outcome (legacy run) is not recovered here",
			outcome:     "",
			result:      "review posted",
			wantHandled: false,
		},
		{
			name:        "unknown outcome falls through instead of forcing failure",
			outcome:     "garbage",
			result:      "review posted",
			wantHandled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

			taskID := newReviewTaskWithHeadlessRun(t, tasks, task.AgentRun{
				AgentID:   "ag-review",
				Role:      "review",
				Mode:      "headless",
				State:     string(agent.StateStopped),
				Outcome:   tc.outcome,
				Result:    tc.result,
				StartedAt: time.Now().Add(-10 * time.Minute),
			})

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

			if !tc.wantHandled {
				if len(wfStub.completions) != 0 {
					t.Fatalf("HandleAgentComplete called %d times; want 0 (no outcome to recover from)", len(wfStub.completions))
				}
				return
			}
			if len(wfStub.completions) != 1 {
				t.Fatalf("HandleAgentComplete called %d times; want 1", len(wfStub.completions))
			}
			got := wfStub.completions[0]
			if got.Success != tc.wantSuccess {
				t.Errorf("Success = %v, want %v", got.Success, tc.wantSuccess)
			}
			if got.Result != tc.result {
				t.Errorf("Result = %q, want %q", got.Result, tc.result)
			}
			_ = taskID
		})
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

// TestPruneTrash_CommitBeforePruneFiresBeforeSweep verifies the ordering
// contract in internal/tasksnapshot's plan: CommitBeforePrune must run
// before Tasks.PruneTrash actually deletes anything, on both the boot-time
// RunStartupCleanup path and the periodic PruneTrash entry point. It backdates
// a real trashed generation past the retention window and has
// CommitBeforePrune observe (via ListTrash) that the generation is still on
// disk at the moment it fires — proving the snapshot commit happens before,
// not just alongside, the bulk-delete sweep.
func TestPruneTrash_CommitBeforePruneFiresBeforeSweep(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	backdate := func(title string) string {
		tk, err := store.Create(title, "body", "headless")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(tk.ID); err != nil {
			t.Fatal(err)
		}
		entries, err := store.ListTrash()
		if err != nil {
			t.Fatal(err)
		}
		var genDir string
		for _, e := range entries {
			if e.ID == tk.ID {
				genDir = filepath.Join(store.TrashDir(), e.DeletedDate, e.Generation)
			}
		}
		if genDir == "" {
			t.Fatalf("could not find trash generation for %s", tk.ID)
		}
		backdated := filepath.Join(store.TrashDir(), time.Now().UTC().AddDate(0, 0, -30).Format(time.DateOnly))
		if err := os.MkdirAll(backdated, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(backdated, filepath.Base(genDir))
		if err := os.Rename(genDir, dst); err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}

	logger := discardLogger()
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        tasks,
		Logger:       logger,
		AgentChecker: agents.HasRunningAgentForTask,
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var calls int
	var sawTrashedAtCommit bool
	var expectID string
	stillTrashed := func(id string) bool {
		entries, err := tasks.ListTrash()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.ID == id {
				return true
			}
		}
		return false
	}
	r := &recovery.Recovery{
		Tasks:     tasks,
		Agents:    agents,
		Worktrees: wm,
		Logger:    logger,
		Throttle:  logging.NewErrorThrottle(),
		WG:        &wg,
		LogDir:    t.TempDir(),
	}
	r.CommitBeforePrune = func(context.Context) {
		mu.Lock()
		calls++
		id := expectID
		mu.Unlock()
		if id != "" && stillTrashed(id) {
			mu.Lock()
			sawTrashedAtCommit = true
			mu.Unlock()
		}
	}

	bootID := backdate("Boot")
	expectID = bootID
	r.RunStartupCleanup(context.Background())
	wg.Wait()

	mu.Lock()
	afterBoot := calls
	sawBoot := sawTrashedAtCommit
	mu.Unlock()
	if afterBoot != 1 {
		t.Fatalf("expected CommitBeforePrune to fire once during RunStartupCleanup, got %d", afterBoot)
	}
	if !sawBoot {
		t.Fatal("expected the backdated generation to still exist when CommitBeforePrune fired (boot path)")
	}
	if stillTrashed(bootID) {
		t.Fatal("expected the backdated generation to be pruned after RunStartupCleanup returned")
	}

	mu.Lock()
	sawTrashedAtCommit = false
	mu.Unlock()
	periodicID := backdate("Periodic")
	expectID = periodicID
	r.PruneTrash(context.Background())

	mu.Lock()
	afterPeriodic := calls
	sawPeriodic := sawTrashedAtCommit
	mu.Unlock()
	if afterPeriodic != 2 {
		t.Fatalf("expected CommitBeforePrune to fire again on the periodic PruneTrash call, got %d", afterPeriodic)
	}
	if !sawPeriodic {
		t.Fatal("expected the backdated generation to still exist when CommitBeforePrune fired (periodic path)")
	}
	if stillTrashed(periodicID) {
		t.Fatal("expected the backdated generation to be pruned after PruneTrash returned")
	}
}

// TestPruneTrash_NilCommitBeforePruneIsSafe verifies the nil-hook path
// (snapshotting disabled/unavailable) never panics.
func TestPruneTrash_NilCommitBeforePruneIsSafe(t *testing.T) {
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
	r.PruneTrash(context.Background())
}

func waitForDeletedRecoveryCWD(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	link := filepath.Join("/proc", strconv.Itoa(pid), "cwd")
	for time.Now().Before(deadline) {
		cwd, err := os.Readlink(link)
		if err == nil && strings.HasSuffix(cwd, " (deleted)") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d cwd never showed deleted suffix", pid)
}

func waitForRecoveryProcessExit(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	for time.Now().Before(deadline) {
		if _, err := os.Stat(procDir); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after startup cleanup", pid)
}
