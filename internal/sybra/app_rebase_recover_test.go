package sybra

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// TestMarkRebaseBlocked_RecoversInsteadOfEscalating verifies that a
// worktree-prep rebase conflict is handed to the conflict-recovery callback
// (the conflict pr-fix) and, when that succeeds, the task is NOT stranded in
// human-required. This is the fix for PRs that have both a merge conflict and a
// failing check: the CI-fix rebase aborts on the conflict, but the conflict is
// recoverable autonomously.
func TestMarkRebaseBlocked_RecoversInsteadOfEscalating(t *testing.T) {
	a := setupApp(t)
	tk, err := a.tasks.Create("rebase recover", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	recoverCalls := 0
	recoverFn := func(id string) bool {
		if id != tk.ID {
			t.Errorf("recover got id %q, want %q", id, tk.ID)
		}
		recoverCalls++
		return true
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !markRebaseBlocked(a.tasks, tk.ID, rebaseErr, a.logger, recoverFn) {
		t.Fatal("markRebaseBlocked returned false for a rebase failure")
	}
	if recoverCalls != 1 {
		t.Fatalf("recover called %d times, want 1", recoverCalls)
	}
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == task.StatusHumanRequired {
		t.Errorf("task escalated to human-required despite successful recovery")
	}
}

// TestMarkRebaseBlocked_EscalatesWhenRecoveryDeclines verifies the fallback:
// when there is no recoverable PR (callback returns false), the task is parked
// in human-required as before.
func TestMarkRebaseBlocked_EscalatesWhenRecoveryDeclines(t *testing.T) {
	a := setupApp(t)
	tk, err := a.tasks.Create("rebase escalate", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !markRebaseBlocked(a.tasks, tk.ID, rebaseErr, a.logger, func(string) bool { return false }) {
		t.Fatal("markRebaseBlocked returned false for a rebase failure")
	}
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required when recovery declines", got.Status)
	}
}

// TestMarkRebaseBlocked_NilRecoverEscalates verifies that a nil recovery
// callback (call sites without a PR-monitor handle) keeps the old behaviour.
func TestMarkRebaseBlocked_NilRecoverEscalates(t *testing.T) {
	a := setupApp(t)
	tk, err := a.tasks.Create("rebase nil recover", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !markRebaseBlocked(a.tasks, tk.ID, rebaseErr, a.logger, nil) {
		t.Fatal("markRebaseBlocked returned false for a rebase failure")
	}
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required with nil recovery", got.Status)
	}
}

// TestMarkRebaseBlocked_IgnoresNonRebaseError verifies non-rebase errors are
// not handled here (returns false so the caller's own error path runs) and the
// recovery callback is never consulted.
func TestMarkRebaseBlocked_IgnoresNonRebaseError(t *testing.T) {
	a := setupApp(t)
	tk, err := a.tasks.Create("non rebase", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	called := false
	if markRebaseBlocked(a.tasks, tk.ID, errors.New("boom"), a.logger, func(string) bool { called = true; return true }) {
		t.Error("markRebaseBlocked returned true for a non-rebase error")
	}
	if called {
		t.Error("recovery callback consulted for a non-rebase error")
	}
}

func TestRecoverStaleBranchConflict_EscalatesWhenWorkflowAlreadyActive(t *testing.T) {
	r, tk := setupRebaseRecoveryReviewHandler(t, false)
	tk.Workflow = &workflow.Execution{
		WorkflowID:  "busy-workflow",
		CurrentStep: "wait",
		State:       workflow.ExecRunning,
	}
	if _, err := r.tasks.Update(tk.ID, task.Update{Workflow: &tk.Workflow}); err != nil {
		t.Fatal(err)
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !markRebaseBlocked(r.tasks, tk.ID, rebaseErr, r.logger, r.recoverStaleBranchConflict) {
		t.Fatal("markRebaseBlocked returned false for a rebase failure")
	}

	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required when recovery dispatch is rejected", got.Status)
	}
	if r.prTracker.Retries(tk.ID, github.PRIssueConflict) != 0 {
		t.Fatal("conflict issue was marked handled even though dispatch was rejected")
	}
}

func TestRecoverStaleBranchConflict_CancelsActiveWorkflowBeforeDispatch(t *testing.T) {
	r, tk := setupRebaseRecoveryReviewHandler(t, true)
	tk.Workflow = &workflow.Execution{
		WorkflowID:  "review-workflow",
		CurrentStep: "code_review_staff",
		State:       workflow.ExecRunning,
	}
	if _, err := r.tasks.Update(tk.ID, task.Update{Workflow: &tk.Workflow}); err != nil {
		t.Fatal(err)
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	handled, recovered := markRebaseBlockedWithRecoveryResult(r.tasks, tk.ID, rebaseErr, r.logger, r.recoverStaleBranchConflict)
	if !handled {
		t.Fatal("markRebaseBlockedWithRecoveryResult returned handled=false for a rebase failure")
	}
	if !recovered {
		t.Fatal("markRebaseBlockedWithRecoveryResult did not report successful recovery")
	}

	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == task.StatusHumanRequired {
		t.Fatalf("status = %q, want conflict workflow dispatch instead of human-required", got.Status)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != "pr-conflict-fix-test" {
		t.Fatalf("workflow = %+v, want pr-conflict-fix-test", got.Workflow)
	}
	if r.prTracker.Retries(tk.ID, github.PRIssueConflict) == 0 {
		t.Fatal("conflict issue was not marked handled after workflow start")
	}
}

func TestRecoverStaleBranchConflict_EscalatesWhenNoWorkflowMatches(t *testing.T) {
	r, tk := setupRebaseRecoveryReviewHandler(t, false)

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !markRebaseBlocked(r.tasks, tk.ID, rebaseErr, r.logger, r.recoverStaleBranchConflict) {
		t.Fatal("markRebaseBlocked returned false for a rebase failure")
	}

	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required when no conflict workflow matches", got.Status)
	}
	if r.prTracker.Retries(tk.ID, github.PRIssueConflict) != 0 {
		t.Fatal("conflict issue was marked handled even though no workflow matched")
	}
}

func TestRecoverStaleBranchConflict_ReturnsTrueWhenWorkflowStarts(t *testing.T) {
	r, tk := setupRebaseRecoveryReviewHandler(t, true)

	if !r.recoverStaleBranchConflict(tk.ID) {
		t.Fatal("recoverStaleBranchConflict returned false when conflict workflow started")
	}

	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != "pr-conflict-fix-test" {
		t.Fatalf("workflow = %+v, want pr-conflict-fix-test", got.Workflow)
	}
	if r.prTracker.Retries(tk.ID, github.PRIssueConflict) == 0 {
		t.Fatal("conflict issue was not marked handled after workflow start")
	}
}

func setupRebaseRecoveryReviewHandler(t *testing.T, withConflictWorkflow bool) (*ReviewHandler, task.Task) {
	t.Helper()

	a := setupApp(t)
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	runGit(t, "", "init", "-b", "main", repo)
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	runGit(t, repo, "checkout", "-b", "feature/recover")

	projects, err := project.NewStore(filepath.Join(tmp, "projects"), filepath.Join(tmp, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	seedExperienceProject(t, filepath.Join(tmp, "projects"), project.Project{
		ID:        "owner/repo",
		Name:      "repo",
		Owner:     "owner",
		Repo:      "repo",
		URL:       "https://github.com/owner/repo",
		ClonePath: repo,
		Type:      project.ProjectTypePet,
	})

	wfStore, err := workflow.NewStore(filepath.Join(tmp, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if withConflictWorkflow {
		if err := wfStore.Save(workflow.Definition{
			ID:   "pr-conflict-fix-test",
			Name: "PR conflict fix test",
			Trigger: workflow.Trigger{On: "pr.event", Conditions: []workflow.Condition{{
				Field: "pr.issue_kind", Operator: "equals", Value: string(github.PRIssueConflict),
			}}},
			Steps: []workflow.Step{{
				ID:   "wait",
				Name: "Wait",
				Type: workflow.StepWaitHuman,
				Config: workflow.StepConfig{
					HumanActions: []string{"done"},
				},
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	engine := workflow.NewEngine(wfStore,
		&taskAdapter{tasks: a.tasks},
		&agentAdapter{agents: a.agents, tasks: a.tasks},
		a.logger,
	)
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler), emit: func(string, any) {}},
		tasks:         a.tasks,
		projects:      projects,
		agents:        a.agents,
		prTracker:     github.NewIssueTracker(0),
		worktrees: worktree.New(worktree.Config{
			WorktreesDir: filepath.Join(tmp, "worktrees"),
			Projects:     projects,
			Tasks:        a.tasks,
			Logger:       slog.New(slog.DiscardHandler),
		}),
		workflowEngine: engine,
		wtFailures:     make(map[string]int),
		fetchKnownPRFn: func(repo string, number int) (github.PullRequest, bool, error) {
			if repo != "owner/repo" || number != 42 {
				return github.PullRequest{}, false, nil
			}
			return github.PullRequest{
				Number:      42,
				Repository:  repo,
				HeadRefName: "feature/recover",
				HeadSHA:     "sha",
			}, true, nil
		},
	}

	tk, err := a.tasks.Create("rebase recover dispatch", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	pr := 42
	tk, err = a.tasks.Update(tk.ID, task.Update{
		ProjectID:   task.Ptr("owner/repo"),
		PRNumber:    &pr,
		WorktreeDir: task.Ptr(repo),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, tk
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
