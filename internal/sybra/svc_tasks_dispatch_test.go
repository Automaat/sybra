package sybra

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/workflow"
)

// fakeAgentLauncher is a minimal workflow.AgentLauncher used to make
// run_agent steps deterministic in tests: StartAgent succeeds or fails per
// startErr without spawning a real provider CLI.
type fakeAgentLauncher struct {
	startErr   error
	startCalls int
}

func (f *fakeAgentLauncher) StartAgent(_, _, _, _, _, _, _ string, _ []string, _, _ bool, _, _ string, _ workflow.AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	f.startCalls++
	if f.startErr != nil {
		return "", "", "", f.startErr
	}
	return "fake-agent-id", "", "", nil
}
func (f *fakeAgentLauncher) TryClaimDispatch(string) (workflow.DispatchClaim, bool) {
	return nil, true
}
func (f *fakeAgentLauncher) HasRunningAgent(string) bool                     { return false }
func (f *fakeAgentLauncher) HasOtherRunningAgentForTask(string, string) bool { return false }
func (f *fakeAgentLauncher) FindRunningAgentForRole(string, string) (string, bool) {
	return "", false
}
func (f *fakeAgentLauncher) StopAgentsForTask(string, string) {}
func (f *fakeAgentLauncher) SendPrompt(string, string) error  { return nil }
func (f *fakeAgentLauncher) DefaultProvider() string          { return "" }
func (f *fakeAgentLauncher) ProviderRateLimited(string) bool  { return false }
func (f *fakeAgentLauncher) ProviderCanFailover(string) bool  { return false }
func (f *fakeAgentLauncher) ProviderHealthy(string) bool      { return true }
func (f *fakeAgentLauncher) IsDispatching(string) bool        { return false }
func (f *fakeAgentLauncher) AdmitDispatch(string, string, string) (admit bool, reason string) {
	return true, ""
}

// setupDispatchTestService builds a TaskService whose workflow engine is
// wired to a fakeAgentLauncher instead of the real agent.Manager-backed
// adapter, so run_agent steps succeed/fail deterministically without a real
// provider CLI on PATH.
func setupDispatchTestService(t *testing.T, launcher *fakeAgentLauncher) (*TaskService, *App) {
	t.Helper()
	svc, a := setupTaskService(t)
	ta := &taskAdapter{tasks: a.tasks}
	svc.workflowEngine = workflow.NewEngine(mustWorkflowStore(t), ta, launcher, a.logger)
	return svc, a
}

func mustWorkflowStore(t *testing.T) *workflow.Store {
	t.Helper()
	store, err := workflow.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.SyncBuiltins(store); err != nil {
		t.Fatal(err)
	}
	return store
}

func newHumanRequiredTask(t *testing.T, a *App, prNumber int) task.Task {
	t.Helper()
	tk, err := a.tasks.Create("fix the thing", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	update := task.Update{Status: task.Ptr(task.StatusHumanRequired)}
	if prNumber > 0 {
		update.PRNumber = task.Ptr(prNumber)
	}
	updated, err := a.tasks.Update(tk.ID, update)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

// fixedWorktreeGetter, fixedPRCreator, and fixedPRLinker are minimal
// scripted workflow.PR* interfaces used only by the "ready-pr" case below.
// ready-pr's tail (push_branch/create_pr) is fully mechanized Go now — no
// run_agent step — so unlike in-progress/testing it needs real PR-tail
// plumbing wired to reach a genuine terminal status instead of escalating to
// human-required for lack of a project/worktree.
type fixedWorktreeGetter struct{ path string }

func (f fixedWorktreeGetter) GetWorktreePath(string) (string, bool) { return f.path, true }

type fixedPRCreator struct {
	number  int
	headSHA string
}

func (f fixedPRCreator) CreatePR(context.Context, string, workflow.PRCreateRequest) (number int, headSHA string, err error) {
	return f.number, f.headSHA, nil
}

type fixedPRLinker struct{}

func (fixedPRLinker) GetClosingIssues(string, int) (issues []int, body string, err error) {
	return nil, "", nil
}
func (fixedPRLinker) EditBody(string, int, string) error { return nil }

// newDispatchTestWorktree creates a bare "origin" clone plus a worktree
// checked out on branch with one commit ahead of it, so push_branch/create_pr
// has real git plumbing to operate on.
func newDispatchTestWorktree(t *testing.T, branch string) string {
	t.Helper()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	src := t.TempDir()
	run(src, "init", "-b", "main")
	run(src, "config", "user.email", "test@test.com")
	run(src, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "add", "README.md")
	run(src, "commit", "-m", "init")

	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := project.CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := project.CreateWorktree(context.Background(), bare, wtPath, branch, "main"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	run(wtPath, "config", "user.email", "test@test.com")
	run(wtPath, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(wtPath, "add", "change.txt")
	run(wtPath, "commit", "-m", "feat: task work")
	return wtPath
}

// newReadyPRHumanRequiredTask wires the shared engine with a full succeeding
// PR-tail and returns a human-required task carrying the project/branch that
// tail expects.
func newReadyPRHumanRequiredTask(t *testing.T, a *App, engine *workflow.Engine) task.Task {
	t.Helper()
	const branch = "feat/existing-pr"
	wtPath := newDispatchTestWorktree(t, branch)
	engine.SetWorktreeGetter(fixedWorktreeGetter{path: wtPath})
	engine.SetPRLinker(fixedPRLinker{})
	sha, err := project.CurrentCommit(context.Background(), wtPath)
	if err != nil {
		t.Fatalf("CurrentCommit: %v", err)
	}
	engine.SetPRCreator(fixedPRCreator{number: 99, headSHA: sha})

	tk, err := a.tasks.Create("fix the thing", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := a.tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("acme/widgets"),
		Branch:    task.Ptr(branch),
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestDispatchFromHumanRequired_HappyPathDispatchingTargets(t *testing.T) {
	for _, target := range []string{"in-progress", "testing"} {
		t.Run(target, func(t *testing.T) {
			launcher := &fakeAgentLauncher{}
			svc, a := setupDispatchTestService(t, launcher)
			tk := newHumanRequiredTask(t, a, 0)

			got, err := svc.DispatchFromHumanRequired(tk.ID, target, "looks fine, retry")
			if err != nil {
				t.Fatalf("DispatchFromHumanRequired: %v", err)
			}
			if string(got.Status) != target {
				t.Fatalf("status = %q, want %q", got.Status, target)
			}
			if got.StatusReason != "looks fine, retry" {
				t.Fatalf("status_reason = %q, want the operator reason", got.StatusReason)
			}
			if got.Workflow == nil {
				t.Fatal("expected a workflow to be attached")
			}
			if launcher.startCalls == 0 {
				t.Fatal("expected the matched workflow to start an agent")
			}
		})
	}

	// ready-pr's tail (push_branch/create_pr/link_pr_and_review/...) is fully
	// mechanized Go — no run_agent step — so unlike the targets above it runs
	// synchronously to a terminal disposition (in-review) instead of pausing
	// at an agent dispatch, and starts zero agents.
	t.Run("ready-pr", func(t *testing.T) {
		launcher := &fakeAgentLauncher{}
		svc, a := setupDispatchTestService(t, launcher)
		tk := newReadyPRHumanRequiredTask(t, a, svc.workflowEngine)

		got, err := svc.DispatchFromHumanRequired(tk.ID, "ready-pr", "looks fine, retry")
		if err != nil {
			t.Fatalf("DispatchFromHumanRequired: %v", err)
		}
		if got.Status != task.StatusInReview {
			t.Fatalf("status = %q, want %q (mechanized PR tail should complete synchronously)", got.Status, task.StatusInReview)
		}
		if got.Workflow == nil {
			t.Fatal("expected a workflow to be attached")
		}
		if launcher.startCalls != 0 {
			t.Fatalf("startCalls = %d, want 0 (create_pr/push_branch are deterministic Go, not agents)", launcher.startCalls)
		}
	})
}

// TestDispatchFromHumanRequired_WithStatusHook reproduces the production wiring
// the other tests miss: App.initStatusHook fires synchronously inside UpdateMap
// and, for testing/ready-pr, already dispatches the workflow via
// dispatchStatusWorkflow before DispatchFromHumanRequired issues its own
// dispatch. Without treating the resulting ErrWorkflowAlreadyActive as success,
// the method reverts the task to human-required while the hook-started agent
// keeps running orphaned. Here the dispatch must succeed and leave the task in
// the target status.
func TestDispatchFromHumanRequired_WithStatusHook(t *testing.T) {
	for _, target := range []string{"testing", "in-progress"} {
		t.Run(target, func(t *testing.T) {
			launcher := &fakeAgentLauncher{}
			svc, a := setupDispatchTestService(t, launcher)
			// Wire the app's real status hook against the SAME engine the
			// service dispatches through, exactly as production does
			// (services_wire_tasks.go shares one engine instance).
			a.workflowEngine = svc.workflowEngine
			a.initStatusHook()

			tk := newHumanRequiredTask(t, a, 0)

			got, err := svc.DispatchFromHumanRequired(tk.ID, target, "looks fine, retry")
			if err != nil {
				t.Fatalf("DispatchFromHumanRequired: %v", err)
			}
			if string(got.Status) != target {
				t.Fatalf("status = %q, want %q (must not revert to human-required)", got.Status, target)
			}
			if got.Workflow == nil {
				t.Fatal("expected a workflow to be attached")
			}
			if launcher.startCalls == 0 {
				t.Fatal("expected the workflow to start an agent")
			}
			// The hook and DispatchFromHumanRequired must not both start an
			// agent — the second attempt is absorbed as ErrWorkflowAlreadyActive.
			if launcher.startCalls != 1 {
				t.Fatalf("startCalls = %d, want exactly 1 (no orphaned double-dispatch)", launcher.startCalls)
			}
		})
	}

	// ready-pr's mechanized tail completes synchronously (see
	// TestDispatchFromHumanRequired_HappyPathDispatchingTargets) — the hook
	// and DispatchFromHumanRequired race to run it, but there is no agent
	// dispatch for either side to double up on.
	t.Run("ready-pr", func(t *testing.T) {
		launcher := &fakeAgentLauncher{}
		svc, a := setupDispatchTestService(t, launcher)
		a.workflowEngine = svc.workflowEngine
		a.initStatusHook()

		tk := newReadyPRHumanRequiredTask(t, a, svc.workflowEngine)

		got, err := svc.DispatchFromHumanRequired(tk.ID, "ready-pr", "looks fine, retry")
		if err != nil {
			t.Fatalf("DispatchFromHumanRequired: %v", err)
		}
		if got.Status != task.StatusInReview {
			t.Fatalf("status = %q, want %q (must not revert to human-required)", got.Status, task.StatusInReview)
		}
		if got.Workflow == nil {
			t.Fatal("expected a workflow to be attached")
		}
		if launcher.startCalls != 0 {
			t.Fatalf("startCalls = %d, want 0 (create_pr/push_branch are deterministic Go, not agents)", launcher.startCalls)
		}
	})
}

// TestApp_StatusHook_SkipsAgentDispatchForUmbrellaTracker reproduces the
// production bug (task 33aa3f62 / issue #2004): app_umbrella_gate.go's
// rollupTrackers rolls an umbrella tracker back to in-progress (e.g. after a
// stuck child resolves), initStatusHook dispatches simple-task-implement for
// that transition same as any other task, the implement step then tries to
// start an agent against a tracker that runs none, and after three rejected
// attempts within 15 minutes the circuit breaker force-escalates the tracker
// to human-required — a state no human retry can actually resolve, since the
// tracker will never successfully run an "implement" agent. initStatusHook
// must skip dispatch entirely for umbrella (and chat) task types, exactly as
// reconcileRunnableBoardTasks already does.
func TestApp_StatusHook_SkipsAgentDispatchForUmbrellaTracker(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	a.workflowEngine = svc.workflowEngine
	a.initStatusHook()

	tk, err := a.tasks.CreateFull("umbrella tracker", "", task.AgentModeHeadless, task.Update{
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusHumanRequired),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
		t.Fatal(err)
	}

	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatalf("workflow = %+v, want nil — an umbrella tracker must never have simple-task-implement dispatched against it", got.Workflow)
	}
	if launcher.startCalls != 0 {
		t.Fatalf("startCalls = %d, want 0 — an umbrella tracker runs no agent", launcher.startCalls)
	}
}

func TestReconcileRunnableBoardTasksDispatchesIdleRunnableStatuses(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	svc.workflowEngine.SetTaskClassifier(&taskClassifierAdapter{
		tasks:      a.tasks,
		classifier: fakeTriageClassifier{},
	})
	a.workflowEngine = svc.workflowEngine

	idle := func(id string, status task.Status) task.Task {
		t.Helper()
		tk, err := a.tasks.Create(id, "", "headless")
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.tasks.UpdateMap(tk.ID, map[string]any{
			"status": string(status),
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	todo := idle("idle-todo", task.StatusTodo)
	planning := idle("idle-planning", task.StatusPlanning)
	inProgress := idle("idle-in-progress", task.StatusInProgress)
	var err error
	inboundReview := idle("inbound-review", task.StatusInReview)
	inboundReview, err = a.tasks.UpdateMap(inboundReview.ID, map[string]any{
		"tags":       []string{"review"},
		"project_id": "Automaat/lightroom-mcp",
		"pr_number":  151,
	})
	if err != nil {
		t.Fatal(err)
	}
	staleInboundReview := idle("stale-inbound-review", task.StatusInReview)
	staleReviewCompletedAt := time.Now().Add(-inboundReviewRedispatchCooldown - time.Minute)
	staleInboundReview, err = a.tasks.UpdateMap(staleInboundReview.ID, map[string]any{
		"tags":         []string{"review"},
		"project_id":   "Automaat/lightroom-mcp",
		"pr_number":    151,
		"review_phase": "needs-approval",
		"workflow": &workflow.Execution{
			WorkflowID:  "pr-review",
			CurrentStep: "",
			State:       workflow.ExecCompleted,
			CompletedAt: &staleReviewCompletedAt,
			Variables:   map[string]string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	approvedInboundReview := idle("approved-inbound-review", task.StatusInReview)
	approvedInboundReview, err = a.tasks.UpdateMap(approvedInboundReview.ID, map[string]any{
		"tags":         []string{"review"},
		"project_id":   "Automaat/lightroom-mcp",
		"pr_number":    152,
		"review_phase": "approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	recentInboundReview := idle("recent-inbound-review", task.StatusInReview)
	recentReviewCompletedAt := time.Now().Add(-inboundReviewRedispatchCooldown / 2)
	recentInboundReview, err = a.tasks.UpdateMap(recentInboundReview.ID, map[string]any{
		"tags":         []string{"review"},
		"project_id":   "Automaat/lightroom-mcp",
		"pr_number":    153,
		"review_phase": "needs-approval",
		"workflow": &workflow.Execution{
			WorkflowID:  "pr-review",
			CurrentStep: "",
			State:       workflow.ExecCompleted,
			CompletedAt: &recentReviewCompletedAt,
			Variables:   map[string]string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ownPRReview := idle("own-pr-review", task.StatusInReview)
	ownPRReview, err = a.tasks.UpdateMap(ownPRReview.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"pr_number":  2037,
	})
	if err != nil {
		t.Fatal(err)
	}
	postPlanInProgress := idle("post-plan-in-progress", task.StatusInProgress)
	postPlanCompletedAt := postPlanInProgress.StatusChangedAt.Add(time.Second)
	postPlanInProgress, err = a.tasks.UpdateMap(postPlanInProgress.ID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "",
			State:       workflow.ExecCompleted,
			CompletedAt: &postPlanCompletedAt,
			Variables:   map[string]string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requeuedPlanning := idle("requeued-planning", task.StatusPlanning)
	requeuedCompletedAt := requeuedPlanning.StatusChangedAt.Add(-time.Hour)
	requeuedPlanning, err = a.tasks.UpdateMap(requeuedPlanning.ID, map[string]any{
		"tags": []string{"backend", "review"},
		"workflow": &workflow.Execution{
			WorkflowID:  "old-workflow",
			CurrentStep: "",
			State:       workflow.ExecCompleted,
			CompletedAt: &requeuedCompletedAt,
			Variables:   map[string]string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	currentTerminal := idle("current-terminal", task.StatusTodo)
	currentCompletedAt := currentTerminal.StatusChangedAt.Add(time.Hour)
	currentTerminal, err = a.tasks.UpdateMap(currentTerminal.ID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID:  "old-workflow",
			CurrentStep: "",
			State:       workflow.ExecCompleted,
			CompletedAt: &currentCompletedAt,
			Variables:   map[string]string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	gatedTodo := idle("gated-todo", task.StatusTodo)
	if _, err := a.tasks.UpdateMap(gatedTodo.ID, map[string]any{
		"tags": []string{umbrella.GatedTag},
	}); err != nil {
		t.Fatal(err)
	}
	prPlanning := idle("pr-planning", task.StatusPlanning)
	if _, err := a.tasks.UpdateMap(prPlanning.ID, map[string]any{
		"pr_number": 123,
		"tags":      []string{"backend", "review"},
	}); err != nil {
		t.Fatal(err)
	}
	umbrellaTask := idle("synthetic-umbrella", task.StatusInProgress)
	if _, err := a.tasks.UpdateMap(umbrellaTask.ID, map[string]any{
		"task_type": string(task.TaskTypeUmbrella),
		"workflow": &workflow.Execution{
			WorkflowID:  "old-workflow",
			CurrentStep: "old-step",
			State:       workflow.ExecFailed,
			Variables:   map[string]string{},
		},
	}); err != nil {
		t.Fatal(err)
	}

	active := idle("active-todo", task.StatusTodo)
	if _, err := a.tasks.UpdateMap(active.ID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "triage",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		},
	}); err != nil {
		t.Fatal(err)
	}

	a.reconcileRunnableBoardTasks(t.Context())
	a.wg.Wait()

	assertWorkflow := func(id, want string) {
		t.Helper()
		got, err := a.tasks.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Workflow == nil || got.Workflow.WorkflowID != want {
			t.Fatalf("%s workflow = %+v, want %s", id, got.Workflow, want)
		}
		if got.Workflow.State == workflow.ExecFailed {
			t.Fatalf("%s kept stale failed workflow: %+v", id, got.Workflow)
		}
	}
	assertWorkflow(todo.ID, "simple-task-plan")
	assertWorkflow(planning.ID, "simple-task-plan")
	assertWorkflow(requeuedPlanning.ID, "simple-task-plan")
	assertWorkflow(inProgress.ID, "simple-task-implement")
	assertWorkflow(postPlanInProgress.ID, "simple-task-implement")
	assertWorkflow(inboundReview.ID, "pr-review")
	assertWorkflow(staleInboundReview.ID, "pr-review")

	gotActive, err := a.tasks.Get(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotActive.Workflow == nil || gotActive.Workflow.WorkflowID != "simple-task-plan" || gotActive.Workflow.State != workflow.ExecWaiting {
		t.Fatalf("active workflow should be left alone, got %+v", gotActive.Workflow)
	}
	gotGated, err := a.tasks.Get(gatedTodo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotGated.Workflow != nil {
		t.Fatalf("umbrella-gated todo task should be left for releaseUnblockedChildren, got %+v", gotGated.Workflow)
	}
	gotPRPlanning, err := a.tasks.Get(prPlanning.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPRPlanning.Workflow != nil {
		t.Fatalf("planning task with PR should not start simple-task-plan, got %+v", gotPRPlanning.Workflow)
	}
	gotOwnPRReview, err := a.tasks.Get(ownPRReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOwnPRReview.Workflow != nil {
		t.Fatalf("ordinary in-review task should stay on PR monitor, got %+v", gotOwnPRReview.Workflow)
	}
	gotApprovedInboundReview, err := a.tasks.Get(approvedInboundReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotApprovedInboundReview.Workflow != nil {
		t.Fatalf("approved inbound review should stay idle, got %+v", gotApprovedInboundReview.Workflow)
	}
	gotRecentInboundReview, err := a.tasks.Get(recentInboundReview.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRecentInboundReview.Workflow == nil ||
		gotRecentInboundReview.Workflow.WorkflowID != "pr-review" ||
		gotRecentInboundReview.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("recent completed inbound review should not re-dispatch, got %+v", gotRecentInboundReview.Workflow)
	}
	gotCurrentTerminal, err := a.tasks.Get(currentTerminal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotCurrentTerminal.Workflow == nil || gotCurrentTerminal.Workflow.WorkflowID != "old-workflow" || gotCurrentTerminal.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("current terminal workflow should be left alone, got %+v", gotCurrentTerminal.Workflow)
	}
	gotUmbrella, err := a.tasks.Get(umbrellaTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUmbrella.Workflow == nil || gotUmbrella.Workflow.WorkflowID != "old-workflow" || gotUmbrella.Workflow.State != workflow.ExecFailed {
		t.Fatalf("synthetic umbrella task should be left alone, got %+v", gotUmbrella.Workflow)
	}
	if launcher.startCalls != 4 {
		t.Fatalf("startCalls = %d, want 4 for idle/post-plan implementation and inbound review tasks", launcher.startCalls)
	}
}

func TestDispatchFromHumanRequired_InReviewFlipsStatusOnly(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	tk := newHumanRequiredTask(t, a, 42)

	got, err := svc.DispatchFromHumanRequired(tk.ID, "in-review", "PR exists, resume monitoring")
	if err != nil {
		t.Fatalf("DispatchFromHumanRequired: %v", err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review", got.Status)
	}
	if launcher.startCalls != 0 {
		t.Fatalf("in-review must not dispatch a workflow, got %d agent starts", launcher.startCalls)
	}
}

func TestDispatchFromHumanRequired_RejectsNonHumanRequiredSource(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	tk, err := a.tasks.Create("normal task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.DispatchFromHumanRequired(tk.ID, "in-progress", "reason"); err == nil {
		t.Fatal("expected error dispatching a non-human-required task")
	}
}

func TestDispatchFromHumanRequired_RejectsEmptyReason(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	tk := newHumanRequiredTask(t, a, 0)

	for _, reason := range []string{"", "   ", "\t\n"} {
		if _, err := svc.DispatchFromHumanRequired(tk.ID, "in-progress", reason); err == nil {
			t.Fatalf("expected error dispatching with reason %q", reason)
		}
	}
	if launcher.startCalls != 0 {
		t.Fatalf("expected no agent starts on rejected reason, got %d", launcher.startCalls)
	}
}

func TestDispatchFromHumanRequired_RejectsUnknownTarget(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	tk := newHumanRequiredTask(t, a, 0)

	if _, err := svc.DispatchFromHumanRequired(tk.ID, "done", "reason"); err == nil {
		t.Fatal("expected error dispatching to an unsupported target")
	}
}

func TestDispatchFromHumanRequired_RejectsInReviewWithoutPR(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	tk := newHumanRequiredTask(t, a, 0)

	if _, err := svc.DispatchFromHumanRequired(tk.ID, "in-review", "reason"); err == nil {
		t.Fatal("expected error dispatching in-review without a linked PR")
	}
	if launcher.startCalls != 0 {
		t.Fatalf("expected no agent starts, got %d", launcher.startCalls)
	}
}

func TestDispatchFromHumanRequired_RejectsRunningAgent(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	tk := newHumanRequiredTask(t, a, 0)

	// Simulate an in-flight dispatch via the manager's claim map instead of
	// starting a real headless agent: spawning a real provider subprocess and
	// racing its (near-instant, doomed-to-fail) exit against this assertion
	// is flaky under CI load. HasRunningAgentForTask treats a held dispatch
	// claim as "running" for exactly this reason.
	if !a.agents.ClaimTaskDispatch(tk.ID) {
		t.Fatal("expected to acquire dispatch claim")
	}
	defer a.agents.ReleaseTaskDispatch(tk.ID)

	if _, err := svc.DispatchFromHumanRequired(tk.ID, "in-progress", "reason"); err == nil {
		t.Fatal("expected error dispatching while an agent is running")
	}
}

func TestDispatchFromHumanRequired_RejectsNonTerminalWorkflow(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	tk := newHumanRequiredTask(t, a, 0)

	wf := &workflow.Execution{WorkflowID: "simple-task-implement", CurrentStep: "implement", State: workflow.ExecWaiting}
	if _, err := a.tasks.Update(tk.ID, task.Update{Workflow: &wf}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.DispatchFromHumanRequired(tk.ID, "in-progress", "reason"); err == nil {
		t.Fatal("expected error dispatching while a workflow is still active")
	}
	if launcher.startCalls != 0 {
		t.Fatalf("expected no agent starts, got %d", launcher.startCalls)
	}
}

func TestDispatchFromHumanRequired_FailsClosedOnNoMatch(t *testing.T) {
	// An empty workflow store never matches any trigger, so DispatchEvent
	// returns ("", nil) and the handler must revert to human-required.
	launcher := &fakeAgentLauncher{}
	svc, a := setupTaskService(t)
	ta := &taskAdapter{tasks: a.tasks}
	emptyStore, err := workflow.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.workflowEngine = workflow.NewEngine(emptyStore, ta, launcher, a.logger)
	tk := newHumanRequiredTask(t, a, 0)

	_, err = svc.DispatchFromHumanRequired(tk.ID, "in-progress", "retry please")
	if err == nil {
		t.Fatal("expected an error when no workflow matches")
	}

	got, getErr := a.tasks.Get(tk.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want revert to human-required", got.Status)
	}
	if !strings.Contains(got.StatusReason, "retry please") {
		t.Fatalf("status_reason = %q, want it to preserve the operator's reason", got.StatusReason)
	}
}

// TestDispatchFromHumanRequired_NilWorkflowEngineFailsClosed reproduces the
// narrow-test-only nil-workflowEngine fallback: with no engine wired,
// dispatching to a dispatching target (in-progress/testing/ready-pr) must
// fail closed via redispatchStatusChanged's nil guard and revert the task
// to human-required, rather than nil-panicking inside DispatchEvent.
func TestDispatchFromHumanRequired_NilWorkflowEngineFailsClosed(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	svc.workflowEngine = nil
	tk := newHumanRequiredTask(t, a, 0)

	_, err := svc.DispatchFromHumanRequired(tk.ID, "in-progress", "retry please")
	if err == nil {
		t.Fatal("expected an error with no workflow engine wired")
	}

	got, getErr := a.tasks.Get(tk.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want revert to human-required", got.Status)
	}
}

func TestDispatchFromHumanRequired_FailsClosedOnDispatchError(t *testing.T) {
	// A genuine agent-start failure. Must NOT be ErrWorkflowAlreadyActive —
	// that sentinel now means "the status-change hook already started the
	// workflow" and is treated as success, not a dispatch failure.
	launcher := &fakeAgentLauncher{startErr: errors.New("provider CLI not found")}
	svc, a := setupDispatchTestService(t, launcher)
	tk := newHumanRequiredTask(t, a, 0)

	_, err := svc.DispatchFromHumanRequired(tk.ID, "in-progress", "retry please")
	if err == nil {
		t.Fatal("expected an error when the agent fails to start")
	}

	got, getErr := a.tasks.Get(tk.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want revert to human-required", got.Status)
	}
	if !strings.Contains(got.StatusReason, "retry please") {
		t.Fatalf("status_reason = %q, want it to preserve the operator's reason", got.StatusReason)
	}
}

// The durable backstop from #2164: a review must never be dispatched twice
// against the same PR head. The phase/cooldown gates above this are all
// functions of local state, so a frozen or wrong review_phase re-opens them
// every cooldown — which is exactly how one unchanged commit collected 112
// reviews. Asking GitHub for the head and refusing a commit we already
// reviewed is what holds when those gates are wrong.
func TestDispatchInboundReview_SkipsAlreadyReviewedHead(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	a.workflowEngine = svc.workflowEngine

	const head = "e57e4b5db72c55ba7610140631a80946a7edddf0"
	var headCalls atomic.Int64
	a.fetchPRHeadSHA = func(context.Context, string, int) (string, error) {
		headCalls.Add(1)
		return head, nil
	}

	tk := newInboundReviewTask(t, a, 151, "needs-approval")
	// This commit's review budget is already spent.
	if _, err := a.tasks.Update(tk.ID, task.Update{
		ReviewedHeadSHA:      task.Ptr(head),
		ReviewedHeadAttempts: task.Ptr(maxReviewAttemptsPerHead),
	}); err != nil {
		t.Fatal(err)
	}

	a.dispatchInboundReviewWorkflow(t.Context(), tk.ID)

	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil && got.Workflow.WorkflowID == "pr-review" && got.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("dispatched a pr-review for an already-reviewed head %s", head)
	}
	if launcher.startCalls != 0 {
		t.Fatalf("startCalls = %d, want 0 — the head was already reviewed", launcher.startCalls)
	}
	if headCalls.Load() == 0 {
		t.Fatal("never asked GitHub for the head; the guard cannot be honest about local state alone")
	}
}

// A genuine new push must still be reviewed — the guard is per-SHA, not a
// blanket reviewed-once flag.
func TestDispatchInboundReview_NewHeadIsReviewed(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	a.workflowEngine = svc.workflowEngine

	a.fetchPRHeadSHA = func(context.Context, string, int) (string, error) { return "new-head-sha", nil }

	tk := newInboundReviewTask(t, a, 151, "needs-approval")
	if _, err := a.tasks.Update(tk.ID, task.Update{
		ReviewedHeadSHA:      task.Ptr("old-head-sha"),
		ReviewedHeadAttempts: task.Ptr(maxReviewAttemptsPerHead),
	}); err != nil {
		t.Fatal(err)
	}

	a.dispatchInboundReviewWorkflow(t.Context(), tk.ID)

	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != "pr-review" {
		t.Fatalf("workflow = %+v, want pr-review — a new push must be reviewed", got.Workflow)
	}
	if got.ReviewedHeadSHA != "new-head-sha" {
		t.Errorf("reviewed_head_sha = %q, want new-head-sha stamped at dispatch", got.ReviewedHeadSHA)
	}
}

// Without the head we cannot tell a new push from the commit we already
// reviewed. Guessing is what produced the incident, so fail closed.
func TestDispatchInboundReview_HeadLookupFailureDefers(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	a.workflowEngine = svc.workflowEngine

	// A non-empty SHA alongside the error: with "" the fail-closed branch in
	// reviewCoversHead would mask a deleted error check, making this tautological.
	a.fetchPRHeadSHA = func(context.Context, string, int) (string, error) {
		return "unreviewed-head-sha", errors.New("gh: HTTP 502")
	}

	tk := newInboundReviewTask(t, a, 151, "needs-approval")
	a.dispatchInboundReviewWorkflow(t.Context(), tk.ID)

	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil && got.Workflow.WorkflowID == "pr-review" {
		t.Fatalf("dispatched despite an unknown head: %+v", got.Workflow)
	}
	if launcher.startCalls != 0 {
		t.Fatalf("startCalls = %d, want 0 — an unknown head must not dispatch", launcher.startCalls)
	}
	if got.ReviewedHeadSHA != "" {
		t.Errorf("reviewed_head_sha = %q, want empty — nothing was reviewed", got.ReviewedHeadSHA)
	}
}

// The incident itself, reduced: review_phase frozen at needs-approval and the
// 10-minute cooldown long expired, so every local gate re-opens — exactly the
// state that re-fired 112 times. The head is unchanged, so the SHA guard is the
// only thing left that can say no.
func TestDispatchInboundReview_CooldownOpenButHeadUnchanged(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	a.workflowEngine = svc.workflowEngine

	const head = "e57e4b5db72c55ba7610140631a80946a7edddf0"
	a.fetchPRHeadSHA = func(context.Context, string, int) (string, error) { return head, nil }

	tk := newInboundReviewTask(t, a, 151, "needs-approval")
	completedAt := time.Now().Add(-inboundReviewRedispatchCooldown - time.Minute)
	if _, err := a.tasks.UpdateMap(tk.ID, map[string]any{
		"reviewed_head_sha":      head,
		"reviewed_head_attempts": maxReviewAttemptsPerHead,
		"workflow": &workflow.Execution{
			WorkflowID: "pr-review", State: workflow.ExecCompleted,
			CompletedAt: &completedAt, Variables: map[string]string{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// The local gates must genuinely be open, or this test proves nothing.
	stale, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !inboundReviewNeedsAgent(stale) {
		t.Fatal("precondition: the phase/cooldown gates must be open for this test to exercise the SHA guard")
	}

	a.dispatchInboundReviewWorkflow(t.Context(), tk.ID)

	if launcher.startCalls != 0 {
		t.Fatalf("startCalls = %d, want 0 — re-reviewed an unchanged head with the cooldown open (the #2164 loop)", launcher.startCalls)
	}
}

func TestReviewCoversHead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		reviewed string
		attempts int
		head     string
		want     bool
	}{
		{"never reviewed", "", 0, "abc", false},
		{"same head, budget left", "abc", 1, "abc", false},
		{"same head, budget spent", "abc", maxReviewAttemptsPerHead, "abc", true},
		{"same head, over budget", "abc", maxReviewAttemptsPerHead + 5, "abc", true},
		{"new head resets the budget", "abc", maxReviewAttemptsPerHead, "def", false},
		{"unknown head fails closed", "abc", 0, "", true},
		{"unknown head with no prior review fails closed", "", 0, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tk := task.Task{ReviewedHeadSHA: tt.reviewed, ReviewedHeadAttempts: tt.attempts}
			if got := reviewCoversHead(tk, tt.head); got != tt.want {
				t.Errorf("reviewCoversHead(sha=%q attempts=%d, head=%q) = %v, want %v",
					tt.reviewed, tt.attempts, tt.head, got, tt.want)
			}
		})
	}
}

func TestNextReviewAttempt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		reviewed string
		attempts int
		head     string
		want     int
	}{
		{"first ever review", "", 0, "abc", 1},
		{"retry on the same head", "abc", 1, "abc", 2},
		{"a new push restarts the budget", "abc", 9, "def", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tk := task.Task{ReviewedHeadSHA: tt.reviewed, ReviewedHeadAttempts: tt.attempts}
			if got := nextReviewAttempt(tk, tt.head); got != tt.want {
				t.Errorf("nextReviewAttempt = %d, want %d", got, tt.want)
			}
		})
	}
}

// The blocking regression the adversary found in the first cut: stamping at
// dispatch and refusing forever turned any transient provider failure into a PR
// that is never reviewed again. A run that fails to start must not spend budget.
func TestDispatchInboundReview_FailedStartDoesNotSpendBudget(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	a.workflowEngine = svc.workflowEngine
	a.fetchPRHeadSHA = func(context.Context, string, int) (string, error) { return "head-1", nil }

	tk := newInboundReviewTask(t, a, 151, "needs-approval")
	// Auto-dispatch off models StartWorkflow refusing before anything runs.
	a.workflowEngine.SetAutoDispatch(false)
	a.dispatchInboundReviewWorkflow(t.Context(), tk.ID)

	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewedHeadSHA != "" || got.ReviewedHeadAttempts != 0 {
		t.Fatalf("charged the head (sha=%q attempts=%d) for a review that never started — "+
			"the PR would never be reviewed again", got.ReviewedHeadSHA, got.ReviewedHeadAttempts)
	}

	// With dispatch re-enabled the review must still happen.
	a.workflowEngine.SetAutoDispatch(true)
	a.dispatchInboundReviewWorkflow(t.Context(), tk.ID)
	got, err = a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewedHeadAttempts != 1 {
		t.Errorf("attempts = %d, want 1 once a run actually started", got.ReviewedHeadAttempts)
	}
}

func newInboundReviewTask(t *testing.T, a *App, prNumber int, phase string) task.Task {
	t.Helper()
	tk, err := a.tasks.Create("Review: external PR", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := a.tasks.UpdateMap(tk.ID, map[string]any{
		"status":       string(task.StatusInReview),
		"tags":         []string{"review"},
		"project_id":   "Automaat/lightroom-mcp",
		"pr_number":    prNumber,
		"review_phase": phase,
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

// An empty SHA with no error means gh returned something we don't understand.
// Declining is right, but it must be visible: a PR quietly never reviewed again
// is precisely what made #2164 hard to diagnose.
func TestDispatchInboundReview_EmptyHeadIsLoggedNotSilent(t *testing.T) {
	launcher := &fakeAgentLauncher{}
	svc, a := setupDispatchTestService(t, launcher)
	a.workflowEngine = svc.workflowEngine

	var buf bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	a.fetchPRHeadSHA = func(context.Context, string, int) (string, error) { return "", nil }

	tk := newInboundReviewTask(t, a, 151, "needs-approval")
	a.dispatchInboundReviewWorkflow(t.Context(), tk.ID)

	if launcher.startCalls != 0 {
		t.Fatalf("startCalls = %d, want 0 — an unknown head must not dispatch", launcher.startCalls)
	}
	if !strings.Contains(buf.String(), "head-empty") {
		t.Errorf("declined silently; logs = %q", buf.String())
	}
}
