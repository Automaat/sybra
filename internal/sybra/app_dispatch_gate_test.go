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
