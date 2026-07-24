package sybra

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

type remoteMirrorFixture struct {
	app           *App
	write         func(task.Task) string
	assignedTasks func() []task.Task
}

func setupRemoteMirrorFixture(t *testing.T) remoteMirrorFixture {
	t.Helper()

	taskSvc, app := setupTaskService(t)
	app.workflowEngine = taskSvc.workflowEngine
	app.ctx = t.Context()

	var (
		mu       sync.Mutex
		assigned []task.Task
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/TaskService/AssignTask":
			var args []task.Task
			_ = json.Unmarshal(body, &args)
			if len(args) == 1 {
				mu.Lock()
				assigned = append(assigned, args[0])
				mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		case "/api/TaskService/ListTasks":
			_ = json.NewEncoder(w).Encode([]task.Task{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "pet-box",
			Endpoints: []string{srv.URL},
			Homes:     []string{"owner/pet"},
		}},
	}}
	roster, err := clusterlead.NewRoster(cfg, nil)
	if err != nil {
		t.Fatalf("NewRoster: %v", err)
	}
	app.cfg = cfg
	app.assigner = clusterlead.NewAssigner(cfg, app.tasks, roster, func(string) bool { return false }, nil, app.logger)

	write := func(tk task.Task) string {
		t.Helper()
		data, err := task.Marshal(tk)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(app.tasksDir, tk.ID+".md")
		if err := fsutil.AtomicWrite(p, data); err != nil {
			t.Fatal(err)
		}
		app.tasks.OnExternalUpdate(p)
		return p
	}

	snapshotAssigned := func() []task.Task {
		mu.Lock()
		defer mu.Unlock()
		return append([]task.Task(nil), assigned...)
	}

	return remoteMirrorFixture{
		app:           app,
		write:         write,
		assignedTasks: snapshotAssigned,
	}
}

// TestApp_MaybeStartWorkflowForExternalTask covers the dispatch path for tasks
// created outside the GUI (e.g. via sybra-cli, which writes the file directly).
// The matching task.created workflow must attach for a fresh todo task, the
// status guard must prevent re-dispatch onto non-fresh tasks, and re-firing
// onto a task that already owns an active workflow must be a no-op.
func TestApp_MaybeStartWorkflowForExternalTask(t *testing.T) {
	taskSvc, app := setupTaskService(t)
	app.workflowEngine = taskSvc.workflowEngine

	// write simulates the cross-process file write sybra-cli performs: it
	// bypasses the in-process task.Manager (AtomicWrite straight to disk),
	// then primes the Manager cache the way app.go's emit callback does.
	write := func(id string, status task.Status, tags ...string) string {
		tk := task.Task{
			ID: id, Title: "ext " + id, Status: status,
			AgentMode: task.AgentModeHeadless, Tags: tags, CreatedAt: time.Now().UTC(),
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
	tk, err := app.tasks.Get("ext-todo")
	if err != nil {
		t.Fatalf("get ext-todo: %v", err)
	}
	if tk.Workflow == nil || tk.Workflow.WorkflowID != "simple-task-plan" {
		t.Fatalf("fresh todo task: expected simple-task-plan workflow attached, got %+v", tk.Workflow)
	}

	// Prompt Lab proposal tasks may be todo, but they are reviewed and
	// advanced by PromptLabService rather than task.created workflows.
	promptLabPath := write("ext-promptlab", task.StatusTodo, promptlab.ProposalTag, "role:review")
	app.maybeStartWorkflowForExternalTask(promptLabPath)
	app.wg.Wait()
	pl, err := app.tasks.Get("ext-promptlab")
	if err != nil {
		t.Fatalf("get ext-promptlab: %v", err)
	}
	if pl.Workflow != nil {
		t.Fatalf("prompt-lab proposal: expected no workflow, got %+v", pl.Workflow)
	}

	// Idempotency: re-firing onto a task that already owns an active workflow
	// must not restart it. Pin a known active workflow, fire again, and assert
	// the step is untouched (DispatchEvent rejects the active workflow and the
	// helper swallows ErrWorkflowAlreadyActive — no double-start, no error).
	activePath := write("ext-active", task.StatusTodo)
	if _, err := app.tasks.UpdateMap("ext-active", map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "triage",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	app.maybeStartWorkflowForExternalTask(activePath)
	app.wg.Wait()
	ak, err := app.tasks.Get("ext-active")
	if err != nil {
		t.Fatalf("get ext-active: %v", err)
	}
	if ak.Workflow == nil || ak.Workflow.CurrentStep != "triage" {
		t.Errorf("re-fire onto active workflow should be a no-op, got %+v", ak.Workflow)
	}

	// Done task → the status guard prevents re-dispatch (simple-task-plan's
	// trigger has no status condition, so without the guard this would restart
	// planning on a finished task).
	donePath := write("ext-done", task.StatusDone)
	app.maybeStartWorkflowForExternalTask(donePath)
	app.wg.Wait()
	dk, err := app.tasks.Get("ext-done")
	if err != nil {
		t.Fatalf("get ext-done: %v", err)
	}
	if dk.Workflow != nil {
		t.Errorf("done task: expected no workflow, got %+v", dk.Workflow)
	}

	// Sidecar files share the tasks dir and also fire TaskCreated — must be a
	// no-op (and must not panic).
	app.maybeStartWorkflowForExternalTask(filepath.Join(app.tasksDir, "ext-todo.plan.md"))
	app.wg.Wait()

	// pr-fix task (RunRole set): must NOT trigger task.created workflow — these
	// are driven by pr.event. Without this guard, simple-task-plan would claim
	// the workflow slot and prevent the pr-fix workflow from starting.
	prfixPath := write("ext-prfix", task.StatusTodo)
	if _, err := app.tasks.UpdateMap("ext-prfix", map[string]any{
		"run_role":  "pr-fix",
		"pr_number": 42,
	}); err != nil {
		t.Fatal(err)
	}
	app.tasks.OnExternalUpdate(prfixPath)
	app.maybeStartWorkflowForExternalTask(prfixPath)
	app.wg.Wait()
	pk, err := app.tasks.Get("ext-prfix")
	if err != nil {
		t.Fatalf("get ext-prfix: %v", err)
	}
	if pk.Workflow != nil {
		t.Errorf("pr-fix task: expected no task.created workflow, got %+v", pk.Workflow)
	}
}

func TestApp_StatusHookRestartsTodoAndPlanningExternalUpdates(t *testing.T) {
	for _, target := range []task.Status{task.StatusTodo, task.StatusPlanning} {
		t.Run(string(target), func(t *testing.T) {
			taskSvc, app := setupTaskService(t)
			app.workflowEngine = taskSvc.workflowEngine
			app.initStatusHook()

			tk := task.Task{
				ID:        "external-retry-" + string(target),
				Title:     "external retry " + string(target),
				Status:    task.StatusHumanRequired,
				AgentMode: task.AgentModeHeadless,
				Workflow: &workflow.Execution{
					WorkflowID:  "simple-task-plan",
					CurrentStep: "triage",
					State:       workflow.ExecFailed,
					Variables:   map[string]string{},
				},
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}
			if target == task.StatusPlanning {
				tk.Tags = []string{"backend", "review"}
			}
			path := filepath.Join(app.tasksDir, tk.ID+".md")
			write := func(in task.Task) {
				t.Helper()
				data, err := task.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				if err := fsutil.AtomicWrite(path, data); err != nil {
					t.Fatal(err)
				}
				app.tasks.OnExternalUpdate(path)
			}

			write(tk)
			tk.Status = target
			tk.UpdatedAt = time.Now().UTC()
			write(tk)
			app.wg.Wait()

			got, err := app.tasks.Get(tk.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Workflow == nil || got.Workflow.WorkflowID != "simple-task-plan" {
				t.Fatalf("external %s update did not restart task.created workflow, got %+v", target, got.Workflow)
			}
			if got.Workflow.State == workflow.ExecFailed {
				t.Fatalf("external %s update kept stale failed workflow: %+v", target, got.Workflow)
			}
		})
	}
}

func TestApp_MaybeStartWorkflowForExternalTask_RemoteMirrorDoesNotReroute(t *testing.T) {
	fixture := setupRemoteMirrorFixture(t)

	mirrored := task.Task{
		ID:           "ext-remote",
		Title:        "remote mirror",
		Status:       task.StatusTodo,
		AgentMode:    task.AgentModeHeadless,
		ProjectID:    "owner/pet",
		AssignedNode: "pet-box",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	path := fixture.write(mirrored)
	fixture.app.maybeStartWorkflowForExternalTask(path)
	fixture.app.wg.Wait()

	// Replaying the same follower-owned todo task must stay idle. This is the
	// leader watcher path that previously fed assignment loops after mirror
	// writes bounced back from the follower.
	mirrored.UpdatedAt = mirrored.UpdatedAt.Add(time.Second)
	path = fixture.write(mirrored)
	fixture.app.maybeStartWorkflowForExternalTask(path)
	fixture.app.wg.Wait()

	got, err := fixture.app.tasks.Get("ext-remote")
	if err != nil {
		t.Fatalf("Get ext-remote: %v", err)
	}
	if got.Workflow != nil {
		t.Fatalf("remote mirror must not start a local workflow, got %+v", got.Workflow)
	}
	if got := fixture.assignedTasks(); len(got) != 0 {
		t.Fatalf("remote mirror re-routed %d times, want 0", len(got))
	}
}

func TestApp_MaybeStartWorkflowForExternalTask_RemoteMirrorBatchStaysIdle(t *testing.T) {
	fixture := setupRemoteMirrorFixture(t)

	const mirroredChildren = 32
	for i := range mirroredChildren {
		mirrored := task.Task{
			ID:           fmt.Sprintf("ext-remote-%02d", i),
			Title:        fmt.Sprintf("remote mirror %02d", i),
			Status:       task.StatusTodo,
			AgentMode:    task.AgentModeHeadless,
			ProjectID:    "owner/pet",
			AssignedNode: "pet-box",
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		path := fixture.write(mirrored)
		fixture.app.maybeStartWorkflowForExternalTask(path)

		mirrored.UpdatedAt = mirrored.UpdatedAt.Add(time.Second)
		path = fixture.write(mirrored)
		fixture.app.maybeStartWorkflowForExternalTask(path)
	}
	fixture.app.wg.Wait()

	for i := range mirroredChildren {
		got, err := fixture.app.tasks.Get(fmt.Sprintf("ext-remote-%02d", i))
		if err != nil {
			t.Fatalf("Get ext-remote-%02d: %v", i, err)
		}
		if got.Workflow != nil {
			t.Fatalf("remote mirror %02d started a local workflow: %+v", i, got.Workflow)
		}
	}
	if got := fixture.assignedTasks(); len(got) != 0 {
		t.Fatalf("remote mirror batch re-routed %d times, want 0", len(got))
	}
}

func TestSkipTaskCreatedWorkflow_PRLinkedHandoffExceptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		task task.Task
		want bool
	}{
		{
			name: "new inbound review task may enter task.created",
			task: task.Task{Status: task.StatusTodo, PRNumber: 42, Tags: []string{"review"}},
			want: false,
		},
		{
			name: "legacy handoff-pr task cannot enter external pr-review lane",
			task: task.Task{Status: task.StatusTodo, PRNumber: 42, Tags: []string{"review", "handoff-pr"}},
			want: true,
		},
		{
			name: "ready-pr worktree handoff may enter task.created lane with known PR",
			task: task.Task{Status: task.StatusTodo, PRNumber: 42, Tags: []string{"handoff", "handoff-ready-pr"}},
			want: false,
		},
		{
			name: "manual raw-status handoff never starts a workflow",
			task: task.Task{Tags: []string{handoffManualTag}},
			want: true,
		},
		{
			name: "umbrella tracker never starts a workflow",
			task: task.Task{TaskType: task.TaskTypeUmbrella},
			want: true,
		},
		{
			name: "run role still skips task.created",
			task: task.Task{RunRole: "pr-fix", PRNumber: 42, Tags: []string{"handoff"}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := skipTaskCreatedWorkflow(tt.task)
			if got != tt.want {
				t.Fatalf("skipTaskCreatedWorkflow() = %v, want %v", got, tt.want)
			}
		})
	}
}
