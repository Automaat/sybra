package synapse

import (
	"context"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/fsutil"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/watcher"
	"github.com/Automaat/synapse/internal/workflow"
)

// TestApp_WatcherStatusHook_AdvancesWorkflow reproduces the production bug
// where two tasks sat permanently in plan-review without ever entering
// critique_plan.
//
// Repro shape:
//   - simple-task workflow parks at the `plan` step waiting for status
//     plan-review (wait_for_status).
//   - The plan agent runs `synapse-cli update --status plan-review` from a
//     separate process that bypasses the in-process task.Manager and writes
//     the file directly.
//   - The watcher fires TaskUpdated. Before the fix, the emit callback only
//     called store.InvalidatePath — it never invoked the status-change
//     hook, so HandleStatusChange never ran and the workflow was stranded.
//
// This test simulates the cross-process write with fsutil.AtomicWrite (the
// same primitive synapse-cli uses) and asserts that the workflow advances
// past `plan` after the file is written.
func TestApp_WatcherStatusHook_AdvancesWorkflow(t *testing.T) {
	taskSvc, app := setupTaskService(t)
	app.workflowEngine = taskSvc.workflowEngine
	app.initStatusHook()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Mirror app.go's emit callback: forward task file events through
	// task.Manager.OnExternalUpdate so cross-process writes participate
	// in the same status-hook plumbing as in-process Manager updates.
	// Mirror app.go's emit callback: forward task file events through
	// task.Manager.OnExternalUpdate so cross-process writes participate
	// in the same status-hook plumbing as in-process Manager updates.
	emit := func(event string, data any) {
		switch event {
		case events.TaskCreated, events.TaskUpdated, events.TaskDeleted:
			if path, ok := data.(string); ok {
				app.tasks.OnExternalUpdate(path)
			}
		}
	}
	w := watcher.New(app.tasksDir, emit, app.logger)
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	<-w.Ready()

	created, err := app.tasks.Create("watcher status hook task", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	// Tag nocritic so the workflow's maybe_critique branch routes directly
	// to review_plan instead of trying to launch a plan-critic agent
	// (which would fail in this lightweight setup).
	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{"tags": []string{"nocritic"}}); err != nil {
		t.Fatal(err)
	}

	// Park the workflow at the simple-task `plan` step. The plan step
	// declares wait_for_status: plan-review.
	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{
		"status": "planning",
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task",
			CurrentStep: "plan",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Snapshot the on-disk task and rewrite it with status=plan-review
	// using AtomicWrite (the same path synapse-cli takes). This bypasses
	// task.Manager entirely, mirroring the cross-process behaviour that
	// caused the bug in production.
	current, err := app.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Status = task.StatusPlanReview
	data, err := task.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.AtomicWrite(current.FilePath, data); err != nil {
		t.Fatal(err)
	}

	// The watcher should fire, the emit callback should detect the
	// status change, and the workflow engine should advance past the
	// `plan` step.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			tk, _ := app.tasks.Get(created.ID)
			step := ""
			state := ""
			if tk.Workflow != nil {
				step = tk.Workflow.CurrentStep
				state = string(tk.Workflow.State)
			}
			t.Fatalf("workflow stuck on step %q (state=%q, status=%q) after external status change to plan-review", step, state, tk.Status)
		case <-time.After(50 * time.Millisecond):
			tk, err := app.tasks.Get(created.ID)
			if err != nil {
				continue
			}
			if tk.Workflow != nil && tk.Workflow.CurrentStep != "plan" {
				return // success — workflow advanced
			}
		}
	}
}
