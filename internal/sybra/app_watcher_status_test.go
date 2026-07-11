package sybra

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/watcher"
	"github.com/Automaat/sybra/internal/workflow"
)

// TestApp_WatcherStatusHook_AdvancesWorkflow reproduces the production bug
// where two tasks sat permanently in plan-review without ever entering
// critique_plan.
//
// Repro shape:
//   - simple-task workflow parks at the `plan` step waiting for status
//     plan-review (wait_for_status).
//   - The plan agent runs `sybra-cli update --status plan-review` from a
//     separate process that bypasses the in-process task.Manager and writes
//     the file directly.
//   - The watcher fires TaskUpdated. Before the fix, the emit callback only
//     called store.InvalidatePath — it never invoked the status-change
//     hook, so HandleStatusChange never ran and the workflow was stranded.
//
// This test simulates the cross-process write with fsutil.AtomicWrite (the
// same primitive sybra-cli uses) and asserts that the workflow advances
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

	// Park the workflow at the simple-task-plan `address_critique` step —
	// the only run_agent step in simple-task-plan that still uses
	// wait_for_status: plan-review (the plan step is now a parallel block
	// of headless one-shots that exit on their own).
	if _, err := app.tasks.UpdateMap(created.ID, map[string]any{
		"status":         "planning",
		"plan":           "plan",
		"plan_critique":  "critique",
		"plan_research":  "research",
		"plan_decisions": "decisions",
		"plan_brief":     "brief",
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "address_critique",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Snapshot the on-disk task and rewrite it with status=plan-review
	// using AtomicWrite (the same path sybra-cli takes). This bypasses
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
			t.Fatalf("workflow stuck on step %q (state=%q, status=%q) after external status change to plan-review (expected advance past address_critique)", step, state, tk.Status)
		case <-time.After(50 * time.Millisecond):
			tk, err := app.tasks.Get(created.ID)
			if err != nil {
				continue
			}
			if tk.Workflow != nil && tk.Workflow.CurrentStep != "address_critique" {
				return // success — workflow advanced
			}
		}
	}
}

func TestApp_WatcherStatusHook_ReleasesTaskAgentsOnExternalHandoffAndTerminal(t *testing.T) {
	tests := []task.Status{task.StatusHumanRequired, task.StatusDone, task.StatusCancelled}
	for _, target := range tests {
		t.Run(string(target), func(t *testing.T) {
			a := setupApp(t)
			var released []string
			a.taskAgentReleaser = func(taskID string) { released = append(released, taskID) }
			a.initStatusHook()

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			emit := func(event string, data any) {
				switch event {
				case events.TaskCreated, events.TaskUpdated, events.TaskDeleted:
					if path, ok := data.(string); ok {
						a.tasks.OnExternalUpdate(path)
					}
				}
			}
			w := watcher.New(a.tasksDir, emit, a.logger)
			if err := w.Start(ctx); err != nil {
				t.Fatal(err)
			}
			<-w.Ready()

			created, err := a.tasks.Create("external release task agents", "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
				t.Fatal(err)
			}

			current, err := a.tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			current.Status = target
			data, err := task.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			if err := fsutil.AtomicWrite(current.FilePath, data); err != nil {
				t.Fatal(err)
			}

			deadline := time.After(5 * time.Second)
			for {
				select {
				case <-deadline:
					t.Fatalf("release not observed after external status change to %s; released=%v", target, released)
				case <-time.After(50 * time.Millisecond):
					if len(released) == 1 && released[0] == created.ID {
						return
					}
				}
			}
		})
	}
}

func TestApp_StatusHook_ReleasesTaskAgentsOnHandoffAndTerminal(t *testing.T) {
	tests := []task.Status{task.StatusHumanRequired, task.StatusDone, task.StatusCancelled}
	for _, target := range tests {
		t.Run(string(target), func(t *testing.T) {
			a := setupApp(t)
			var released []string
			a.taskAgentReleaser = func(taskID string) { released = append(released, taskID) }
			a.initStatusHook()

			created, err := a.tasks.Create("release task agents", "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
				t.Fatal(err)
			}

			if _, err := a.tasks.Update(created.ID, task.Update{Status: task.Ptr(target)}); err != nil {
				t.Fatal(err)
			}

			if len(released) != 1 || released[0] != created.ID {
				t.Fatalf("released = %v, want [%s]", released, created.ID)
			}
		})
	}
}

// TestApp_StatusHook_ReadyReview_DispatchesReviewWorkflow verifies that
// initStatusHook dispatches simple-task-review when a task is manually
// moved to ready-review. Before this fix the ready-review case was absent
// from the switch, so the hook silently no-opped and simple-task-review
// never ran on manual re-entry.
func TestApp_StatusHook_ReadyReview_DispatchesReviewWorkflow(t *testing.T) {
	a := setupApp(t)

	// Build a custom workflow store containing only a lightweight version of
	// simple-task-review that completes without spawning agents. The real
	// builtin has run_agent steps that require a live claude binary.
	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	const testReviewWF = `id: simple-task-review
name: Test Review
trigger:
  on: task.status_changed
  conditions:
    - field: task.status
      operator: equals
      value: ready-review
steps:
  - id: mark_testing
    name: Hand to Testing
    type: set_status
    config:
      status: testing
    next:
      - goto: ""
`
	if err := os.WriteFile(filepath.Join(wfDir, "simple-task-review.yaml"), []byte(testReviewWF), 0o644); err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: a.tasks}
	aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}
	a.workflowEngine = workflow.NewEngine(wfStore, ta, aa, a.logger)
	a.initStatusHook()

	created, err := a.tasks.Create("ready-review dispatch", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Act — move to ready-review, mirroring a manual sybra-cli update.
	if _, err := a.tasks.UpdateMap(created.ID, map[string]any{
		"status": string(task.StatusReadyReview),
	}); err != nil {
		t.Fatal(err)
	}

	// Assert — the hook must have dispatched simple-task-review, which runs
	// synchronously (single set_status step, no agents) and completes before
	// UpdateMap returns.
	tk, err := a.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow == nil {
		t.Fatal("no workflow attached — initStatusHook did not dispatch simple-task-review on ready-review")
	}
	if tk.Workflow.WorkflowID != "simple-task-review" {
		t.Errorf("workflow.id = %q, want simple-task-review", tk.Workflow.WorkflowID)
	}
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Errorf("workflow.state = %q, want ExecCompleted", tk.Workflow.State)
	}
	// The test workflow's single step flips status to testing.
	if tk.Status != task.StatusTesting {
		t.Errorf("task status = %q, want testing (set_status step in test workflow)", tk.Status)
	}
}

// TestApp_StatusHook_Testing_DispatchesTestingWorkflow verifies that
// initStatusHook dispatches testing-task when a task is manually moved to
// testing. This covers the hook-level path that direct workflow-engine tests
// bypass.
func TestApp_StatusHook_Testing_DispatchesTestingWorkflow(t *testing.T) {
	a := setupApp(t)

	// Lightweight testing-task that completes without spawning agents — the
	// real builtin's test runner step needs a live provider binary.
	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	const testTestingWF = `id: testing-task
name: Test Testing
trigger:
  on: task.status_changed
  conditions:
    - field: task.status
      operator: equals
      value: testing
steps:
  - id: mark_ready_pr
    name: Hand to PR
    type: set_status
    config:
      status: ready-pr
    next:
      - goto: ""
`
	if err := os.WriteFile(filepath.Join(wfDir, "testing-task.yaml"), []byte(testTestingWF), 0o644); err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: a.tasks}
	aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}
	a.workflowEngine = workflow.NewEngine(wfStore, ta, aa, a.logger)
	a.initStatusHook()

	created, err := a.tasks.Create("testing dispatch", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Act — move to testing, mirroring a manual sybra-cli/UI recovery.
	if _, err := a.tasks.UpdateMap(created.ID, map[string]any{
		"status": string(task.StatusTesting),
	}); err != nil {
		t.Fatal(err)
	}

	// Assert — the hook must have dispatched testing-task, which runs
	// synchronously (single set_status step) and completes before UpdateMap
	// returns.
	tk, err := a.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow == nil {
		t.Fatal("no workflow attached — initStatusHook did not dispatch testing-task on testing")
	}
	if tk.Workflow.WorkflowID != "testing-task" {
		t.Errorf("workflow.id = %q, want testing-task", tk.Workflow.WorkflowID)
	}
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Errorf("workflow.state = %q, want ExecCompleted", tk.Workflow.State)
	}
	// The test workflow's single step flips status to ready-pr.
	if tk.Status != task.StatusReadyPR {
		t.Errorf("task status = %q, want ready-pr (set_status step in test workflow)", tk.Status)
	}
}

// TestApp_StatusHook_ReadyPR_DispatchesPRWorkflow verifies that initStatusHook
// dispatches simple-task-pr when a task is moved to ready-pr. This reproduces
// the production strand where tasks that hit a tester infra failure were
// manually flipped to ready-pr (via sybra-cli or the UI) but no PR ever opened:
// the switch handled testing and ready-review but had no ready-pr case, so the
// hook silently no-opped and simple-task-pr never ran on re-entry.
func TestApp_StatusHook_ReadyPR_DispatchesPRWorkflow(t *testing.T) {
	a := setupApp(t)

	// Lightweight simple-task-pr that completes without spawning agents — the
	// real builtin's create_pr step needs a live gh/claude binary.
	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	const testPRWF = `id: simple-task-pr
name: Test Open PR
trigger:
  on: task.status_changed
  conditions:
    - field: task.status
      operator: equals
      value: ready-pr
steps:
  - id: mark_in_review
    name: Hand to In Review
    type: set_status
    config:
      status: in-review
    next:
      - goto: ""
`
	if err := os.WriteFile(filepath.Join(wfDir, "simple-task-pr.yaml"), []byte(testPRWF), 0o644); err != nil {
		t.Fatal(err)
	}

	ta := &taskAdapter{tasks: a.tasks}
	aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}
	a.workflowEngine = workflow.NewEngine(wfStore, ta, aa, a.logger)
	a.initStatusHook()

	created, err := a.tasks.Create("ready-pr dispatch", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Act — move to ready-pr, mirroring a manual sybra-cli/UI recovery.
	if _, err := a.tasks.UpdateMap(created.ID, map[string]any{
		"status": string(task.StatusReadyPR),
	}); err != nil {
		t.Fatal(err)
	}

	// Assert — the hook must have dispatched simple-task-pr, which runs
	// synchronously (single set_status step) and completes before UpdateMap
	// returns.
	tk, err := a.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow == nil {
		t.Fatal("no workflow attached — initStatusHook did not dispatch simple-task-pr on ready-pr")
	}
	if tk.Workflow.WorkflowID != "simple-task-pr" {
		t.Errorf("workflow.id = %q, want simple-task-pr", tk.Workflow.WorkflowID)
	}
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Errorf("workflow.state = %q, want ExecCompleted", tk.Workflow.State)
	}
	// The test workflow's single step flips status to in-review.
	if tk.Status != task.StatusInReview {
		t.Errorf("task status = %q, want in-review (set_status step in test workflow)", tk.Status)
	}
}
