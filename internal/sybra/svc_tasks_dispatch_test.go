package sybra

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
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
