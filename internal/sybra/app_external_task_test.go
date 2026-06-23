package sybra

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/task"
)

// TestApp_MaybeStartWorkflowForExternalTask covers the dispatch path for tasks
// created outside the GUI (e.g. via sybra-cli, which writes the file directly).
// The matching task.created workflow must attach for a fresh todo task, and the
// status guard must prevent re-dispatch onto non-fresh tasks.
func TestApp_MaybeStartWorkflowForExternalTask(t *testing.T) {
	taskSvc, app := setupTaskService(t)
	app.workflowEngine = taskSvc.workflowEngine

	// write simulates the cross-process file write sybra-cli performs: it
	// bypasses the in-process task.Manager (AtomicWrite straight to disk),
	// then primes the Manager cache the way app.go's emit callback does.
	write := func(id string, status task.Status) string {
		tk := task.Task{
			ID: id, Title: "ext " + id, Status: status,
			AgentMode: task.AgentModeHeadless, CreatedAt: time.Now().UTC(),
		}
		data, err := task.Marshal(tk)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(app.tasksDir, id+".md")
		if err := fsutil.AtomicWrite(p, data); err != nil {
			t.Fatal(err)
		}
		app.tasks.OnExternalUpdate(p)
		return p
	}

	// Fresh todo task → the task.created workflow (simple-task-plan) attaches.
	todoPath := write("ext-todo", task.StatusTodo)
	app.maybeStartWorkflowForExternalTask(todoPath)
	app.wg.Wait()
	if tk, _ := app.tasks.Get("ext-todo"); tk.Workflow == nil || tk.Workflow.WorkflowID != "simple-task-plan" {
		t.Fatalf("fresh todo task: expected simple-task-plan workflow attached, got %+v", tk.Workflow)
	}

	// Done task → the status guard prevents re-dispatch (simple-task-plan's
	// trigger has no status condition, so without the guard this would restart
	// planning on a finished task).
	donePath := write("ext-done", task.StatusDone)
	app.maybeStartWorkflowForExternalTask(donePath)
	app.wg.Wait()
	if tk, _ := app.tasks.Get("ext-done"); tk.Workflow != nil {
		t.Errorf("done task: expected no workflow, got %+v", tk.Workflow)
	}

	// Sidecar files share the tasks dir and also fire TaskCreated — must be a
	// no-op (and must not panic).
	app.maybeStartWorkflowForExternalTask(filepath.Join(app.tasksDir, "ext-todo.plan.md"))
	app.wg.Wait()
}
