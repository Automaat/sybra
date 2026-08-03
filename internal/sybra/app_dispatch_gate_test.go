package sybra

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// TestDispatchTaskCreatedWorkflow_RefusesDuringStartupRecovery pins #2752: the
// file watcher and status hook go live before RunStartupCleanup reattaches
// survivor agents, so a task.created event observed in that window must not
// reach the workflow engine — HasRunningAgentForTask would read an empty
// registry and risk starting a duplicate agent on a live worktree. Once
// startupRecoveryPending clears, the same event must dispatch normally.
func TestDispatchTaskCreatedWorkflow_RefusesDuringStartupRecovery(t *testing.T) {
	app := setupApp(t)
	launcher := &recordingAgentLauncher{}
	app.workflowEngine = workflow.NewEngine(
		mustWorkflowStoreWithTestSimple(t, t.TempDir()),
		&taskAdapter{tasks: app.tasks},
		launcher,
		discardLogger(),
	)

	created, err := app.tasks.Create("dispatch gate test", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	app.startupRecoveryPending.Store(true)
	app.dispatchTaskCreatedWorkflow(created.ID)
	app.wg.Wait()
	if len(launcher.calls) != 0 {
		t.Fatalf("dispatchTaskCreatedWorkflow dispatched while startup recovery was pending: calls=%v", launcher.calls)
	}

	app.startupRecoveryPending.Store(false)
	app.dispatchTaskCreatedWorkflow(created.ID)
	app.wg.Wait()
	if len(launcher.calls) != 1 || launcher.calls[0] != created.ID {
		t.Fatalf("dispatchTaskCreatedWorkflow calls = %v, want one call for %s once recovery cleared", launcher.calls, created.ID)
	}
}

// TestDispatchStatusWorkflow_RefusesDuringStartupRecovery mirrors the above
// for the status-hook dispatch sink, using a minimal task.status_changed
// trigger so the assertion doesn't depend on a run_agent step / launcher.
func TestDispatchStatusWorkflow_RefusesDuringStartupRecovery(t *testing.T) {
	home := t.TempDir()
	store, err := task.NewStore(filepath.Join(home, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	created, err := taskMgr.Create("status dispatch gate test", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := taskMgr.Update(created.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	wfDir := filepath.Join(home, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const yaml = `id: status-changed-gate-test
name: Status Changed Gate Test
trigger:
  on: task.status_changed
steps:
  - id: mark_done
    type: set_status
    config:
      status: done
`
	if err := os.WriteFile(filepath.Join(wfDir, "status-changed-gate-test.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}

	engine := workflow.NewEngine(wfStore, &taskAdapter{tasks: taskMgr}, &recordingAgentLauncher{}, discardLogger())
	app := &App{tasks: taskMgr, workflowEngine: engine, logger: discardLogger()}

	app.startupRecoveryPending.Store(true)
	app.dispatchStatusWorkflow(created.ID, task.StatusInProgress)
	got, err := taskMgr.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q after dispatch while startup recovery was pending, want unchanged %q", got.Status, task.StatusInProgress)
	}

	app.startupRecoveryPending.Store(false)
	app.dispatchStatusWorkflow(created.ID, task.StatusInProgress)
	got, err = taskMgr.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q after dispatch once recovery cleared, want %q", got.Status, task.StatusDone)
	}
}

// TestStatusHook_WaitForStatus_RefusesDuringStartupRecovery pins the
// remaining gap in #2752: initStatusHook called workflowEngine.HandleStatusChange
// unconditionally (only dispatchTaskCreatedWorkflow/dispatchStatusWorkflow/
// dispatchPlanningWorkflow were gated), so a parked run_agent step's
// wait_for_status could still advance — and step completion can itself
// trigger a dispatch (e.g. the next run_agent step) — while
// RunStartupCleanup has not yet reattached survivor agents. Reuses the
// waitForStatusWorkflowYAML fixture (parks on step "plan" until status
// becomes plan-review) driven through the real status-change hook, not a
// direct engine call, so it covers the App-level gate.
func TestStatusHook_WaitForStatus_RefusesDuringStartupRecovery(t *testing.T) {
	app := setupApp(t)

	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "test-status-hook.yaml"), []byte(waitForStatusWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: app.tasks},
		&agentAdapter{agents: app.agents, agentOrch: app.agentOrch, tasks: app.tasks},
		app.logger,
	)
	app.workflowEngine = engine
	app.initStatusHook()

	parkOnPlanStep := func(id string) {
		t.Helper()
		if _, err := app.tasks.UpdateMap(id, map[string]any{
			"status": "planning",
			"workflow": &workflow.Execution{
				WorkflowID:  "test-status-hook",
				CurrentStep: "plan",
				State:       workflow.ExecWaiting,
				Variables:   map[string]string{},
			},
		}); err != nil {
			t.Fatalf("UpdateMap: %v", err)
		}
	}

	// Blocked: startup recovery still pending, the status flip into
	// plan-review must not advance the parked step.
	blocked, err := app.tasks.Create("status hook gate test (blocked)", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	parkOnPlanStep(blocked.ID)

	app.startupRecoveryPending.Store(true)
	if _, err := app.tasks.Update(blocked.ID, task.Update{Status: task.Ptr(task.StatusPlanReview)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := app.tasks.Get(blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.CurrentStep != "plan" {
		t.Fatalf("workflow = %+v after status-hook event while startup recovery was pending, want unchanged at step 'plan'", got.Workflow)
	}

	// Allowed: once startup recovery clears, the identical transition on a
	// fresh task advances past the parked step as before.
	allowed, err := app.tasks.Create("status hook gate test (allowed)", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	parkOnPlanStep(allowed.ID)

	app.startupRecoveryPending.Store(false)
	if _, err := app.tasks.Update(allowed.ID, task.Update{Status: task.Ptr(task.StatusPlanReview)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = app.tasks.Get(allowed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.CurrentStep == "plan" {
		t.Fatalf("workflow = %+v after status-hook event once recovery cleared, want advanced past step 'plan'", got.Workflow)
	}

	// Suppressed is not dropped: the blocked task's awaited status is still the
	// persisted one, so the replay run after the gate clears must advance it.
	// Nothing else would — agent completion deliberately skips wait_for_status
	// steps and board reconciliation skips tasks with an active workflow.
	app.replayDeferredStatusChanges()
	got, err = app.tasks.Get(blocked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.CurrentStep == "plan" {
		t.Fatalf("workflow = %+v after replaying deferred status changes, want advanced past step 'plan'", got.Workflow)
	}
}

// TestReplayDeferredStatusChanges_UsesCurrentStatus pins the coalescing rule:
// the replay re-reads the task's persisted status instead of replaying the
// status recorded at suppression time, so a step waiting on a status the task
// has since left is not advanced by a stale event.
func TestReplayDeferredStatusChanges_UsesCurrentStatus(t *testing.T) {
	app := setupApp(t)

	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "test-status-hook.yaml"), []byte(waitForStatusWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	app.workflowEngine = workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: app.tasks},
		&agentAdapter{agents: app.agents, agentOrch: app.agentOrch, tasks: app.tasks},
		app.logger,
	)
	app.initStatusHook()

	created, err := app.tasks.Create("status hook replay test", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{
		"status": "planning",
		"workflow": &workflow.Execution{
			WorkflowID:  "test-status-hook",
			CurrentStep: "plan",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		},
	}); err != nil {
		t.Fatalf("UpdateMap: %v", err)
	}

	app.startupRecoveryPending.Store(true)
	if _, err := app.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusPlanReview)}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Moved on again inside the window: the awaited plan-review no longer holds.
	if _, err := app.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	app.startupRecoveryPending.Store(false)
	app.replayDeferredStatusChanges()

	got, err := app.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.CurrentStep != "plan" {
		t.Fatalf("workflow = %+v after replay, want still parked at step 'plan' (task left the awaited status)", got.Workflow)
	}
}

// TestAppStartup_ClearsStartupRecoveryPending pins the wiring: Startup arms
// dispatch (startupRecoveryPending -> false) only after RunStartupCleanup
// returns, not before.
func TestAppStartup_ClearsStartupRecoveryPending(t *testing.T) {
	preventFetchTTLLeak(t)
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_DISABLE_WORKFLOWS", "0")

	cfg := startupTestConfig(home)
	app := NewApp(discardLogger(), &slog.LevelVar{}, cfg)
	if err := app.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	t.Cleanup(func() {
		if app.agentSvc != nil && app.agentSvc.approval != nil {
			_ = app.agentSvc.approval.Shutdown(context.Background())
		}
		app.Shutdown(context.Background())
	})

	// The flag must be armed the moment Startup returns: Store(false) only
	// happens inside the recovery goroutine after RunStartupCleanup returns,
	// which does real filesystem work and cannot complete before the two
	// trivial statements between startLifecycle and Startup's return. Without
	// this assertion, deleting Store(true) would leave the zero-value false and
	// the eventual-clear poll below would pass identically — catching nothing.
	if app.startupRecoveryDone() {
		t.Fatal("startupRecoveryPending was not armed when Startup returned — dispatch gate fails open during recovery")
	}

	deadline := time.After(5 * time.Second)
	for {
		if app.startupRecoveryDone() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for startupRecoveryPending to clear after RunStartupCleanup")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
