package review

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// newRebaseRecoveryDeps builds the minimal task/agent/logger trio these
// rebase-recovery tests need, without going through package sybra's App.
func newRebaseRecoveryDeps(t *testing.T) (tasks *task.Manager, agents *agent.Manager, logger *slog.Logger) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sybra-test-tasks-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks = task.NewManager(store, nil)

	logger = slog.New(slog.DiscardHandler)
	agents = newTestAgentManager(t, t.Context(), func(string, any) {}, logger, filepath.Join(t.TempDir(), "logs"))
	return tasks, agents, logger
}

func TestRecoverStaleBranchConflict_EscalatesWhenWorkflowAlreadyActive(t *testing.T) {
	r, tk := setupRebaseRecoveryHandler(t, false)
	tk.Workflow = &workflow.Execution{
		WorkflowID:  "busy-workflow",
		CurrentStep: "wait",
		State:       workflow.ExecRunning,
	}
	if _, err := r.tasks.Update(tk.ID, task.Update{Workflow: &tk.Workflow}); err != nil {
		t.Fatal(err)
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !agentorch.MarkRebaseBlocked(r.tasks, tk.ID, rebaseErr, r.logger, r.RecoverStaleBranchConflict) {
		t.Fatal("agentorch.MarkRebaseBlocked returned false for a rebase failure")
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

func TestRecoverStaleBranchConflict_ReplacesActiveWorkflowDuringDispatch(t *testing.T) {
	r, tk := setupRebaseRecoveryHandler(t, true)
	tk.Workflow = &workflow.Execution{
		WorkflowID:  "review-workflow",
		CurrentStep: "code_review_staff",
		State:       workflow.ExecRunning,
	}
	if _, err := r.tasks.Update(tk.ID, task.Update{Workflow: &tk.Workflow}); err != nil {
		t.Fatal(err)
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	handled, recovered := agentorch.MarkRebaseBlockedWithRecoveryResult(r.tasks, tk.ID, rebaseErr, r.logger, r.RecoverStaleBranchConflict)
	if !handled {
		t.Fatal("agentorch.MarkRebaseBlockedWithRecoveryResult returned handled=false for a rebase failure")
	}
	if !recovered {
		t.Fatal("agentorch.MarkRebaseBlockedWithRecoveryResult did not report successful recovery")
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
	r, tk := setupRebaseRecoveryHandler(t, false)

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !agentorch.MarkRebaseBlocked(r.tasks, tk.ID, rebaseErr, r.logger, r.RecoverStaleBranchConflict) {
		t.Fatal("agentorch.MarkRebaseBlocked returned false for a rebase failure")
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
	r, tk := setupRebaseRecoveryHandler(t, true)

	if !r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("RecoverStaleBranchConflict returned false when conflict workflow started")
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

func setupRebaseRecoveryHandler(t *testing.T, withConflictWorkflow bool) (*Handler, task.Task) {
	t.Helper()

	tasks, agents, logger := newRebaseRecoveryDeps(t)
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
		if err := wfStore.Save(workflow.Definition{
			ID:   "existing-pr-push-test",
			Name: "Existing PR push test",
			Trigger: workflow.Trigger{On: "task.status_changed", Conditions: []workflow.Condition{{
				Field: "task.status", Operator: "equals", Value: string(task.StatusReadyPR),
			}}},
			Steps: []workflow.Step{{
				ID:   "push_existing_pr",
				Name: "Push Existing PR",
				Type: workflow.StepPushBranch,
				Next: []workflow.Transition{{GoTo: ""}},
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	engine := workflow.NewEngine(wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agents, tasks: tasks},
		logger,
	)
	r := &Handler{
		logger: slog.New(slog.DiscardHandler), emit: func(string, any) {},
		tasks:     tasks,
		projects:  projects,
		agents:    agents,
		prTracker: github.NewIssueTracker(0),
		worktrees: worktree.New(worktree.Config{
			WorktreesDir: filepath.Join(tmp, "worktrees"),
			Projects:     projects,
			Tasks:        tasks,
			Logger:       slog.New(slog.DiscardHandler),
		}),
		WorkflowEngine: engine,
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

	tk, err := tasks.Create("rebase recover dispatch", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	pr := 42
	tk, err = tasks.Update(tk.ID, task.Update{
		ProjectID:   task.Ptr("owner/repo"),
		PRNumber:    &pr,
		WorktreeDir: task.Ptr(repo),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, tk
}

type staticWorktreeGetter struct {
	path string
	ok   bool
}

func (g staticWorktreeGetter) GetWorktreePath(string) (string, bool) {
	return g.path, g.ok
}

func commitRecoveryTestFile(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", name)
	runGit(t, repo, "commit", "-m", "add "+name)
}

func makeExistingPRBranchDiverged(t *testing.T, repo string) {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "init", "--bare", remote)
	runGit(t, repo, "remote", "add", "origin", remote)

	commitRecoveryTestFile(t, repo, "one.txt", "one\n")
	commitRecoveryTestFile(t, repo, "two.txt", "two\n")
	runGit(t, repo, "push", "-u", "origin", "feature/recover")
	runGit(t, repo, "reset", "--hard", "HEAD~1")
	commitRecoveryTestFile(t, repo, "two-prime.txt", "two-prime\n")
}

func TestRecoverStaleBranchConflict_ReplacesExistingPRWorkflowReentrantly(t *testing.T) {
	r, tk := setupRebaseRecoveryHandler(t, true)
	makeExistingPRBranchDiverged(t, tk.WorktreeDir)

	r.WorkflowEngine.SetWorktreeGetter(staticWorktreeGetter{path: tk.WorktreeDir, ok: true})
	r.WorkflowEngine.SetDivergenceRecovery(r.RecoverStaleBranchConflict)

	if err := r.WorkflowEngine.StartWorkflowWithVars(tk.ID, "existing-pr-push-test", nil); err != nil {
		t.Fatalf("StartWorkflowWithVars(existing-pr-push-test): %v", err)
	}

	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == task.StatusHumanRequired {
		t.Fatalf("status = %q, want reentrant recovery to take over existing PR workflow", got.Status)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != "pr-conflict-fix-test" {
		t.Fatalf("workflow = %+v, want pr-conflict-fix-test", got.Workflow)
	}
	if got.Workflow.CurrentStep != "wait" {
		t.Fatalf("current step = %q, want wait", got.Workflow.CurrentStep)
	}
	if r.prTracker.Retries(tk.ID, github.PRIssueConflict) == 0 {
		t.Fatal("conflict issue was not marked handled after reentrant workflow replacement")
	}
}

// mechanicalBranchConflictFixYAML is a stand-in for the real
// branch-conflict-fix builtin (internal/workflow/builtin/branch-conflict-fix.yaml)
// with its run_agent step removed, mirroring the existing
// mechanicalPRFixYAML/wait_human convention used elsewhere in this package's
// tests (see setupRebaseRecoveryHandler's "pr-conflict-fix-test"
// stand-in) to dispatch a real recovery workflow deterministically without
// spawning a real agent process. Exercises the same set_status ->
// resume_workflow chain the real workflow ends with; the run_agent/
// route_pr_fix_result/verify_commits/detect_tampering steps this omits are
// covered by the workflow package's own tests (TestBuiltinDefinitions_Valid
// validates the real production YAML parses; TestExecResumeWorkflow_*
// exercises resume_workflow directly).
const branchConflictFixTestWorkflowID = "branch-conflict-fix"

// setupBranchConflictNoPRHandler mirrors setupRebaseRecoveryHandler
// but for the no-PR path: the task carries no PRNumber, and its branch
// already exists (checked out, then returned to main so a managed worktree
// can be added for it) so PrepareForBranchFix can resolve it by name instead
// of via a PR lookup.
func setupBranchConflictNoPRHandler(t *testing.T, initialStatus task.Status, priorWorkflow *workflow.Execution) (*Handler, task.Task) {
	t.Helper()

	tasks, agents, logger := newRebaseRecoveryDeps(t)
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
	runGit(t, repo, "checkout", "-b", "feature/no-pr-recover")
	if err := os.WriteFile(filepath.Join(repo, "work.md"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "work.md")
	runGit(t, repo, "commit", "-m", "task work")
	// Leave the branch un-checked-out so a managed worktree can be added for
	// it (git refuses to check out a branch already checked out elsewhere).
	runGit(t, repo, "checkout", "main")

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
	if err := wfStore.Save(workflow.Definition{
		ID:   branchConflictFixTestWorkflowID,
		Name: "Branch conflict fix (test stand-in)",
		Trigger: workflow.Trigger{On: "task.status_changed", Conditions: []workflow.Condition{{
			Field: "task.tags", Operator: "contains", Value: "__never_dispatch_branch_conflict_fix_test__",
		}}},
		Steps: []workflow.Step{
			{
				ID:   "set_recovering",
				Name: "Mark Recovering",
				Type: workflow.StepSetStatus,
				Config: workflow.StepConfig{
					Status:       "in-progress",
					StatusReason: "recovering from a branch conflict (no PR yet)",
				},
				Next: []workflow.Transition{{GoTo: "await_fix"}},
			},
			// A wait_human stand-in for the real workflow's run_agent "fix"
			// step: like run_agent, it is an async step that breaks the
			// synchronous chain, matching the real workflow's structure
			// (resume_original always sits behind an async step) closely
			// enough for this test — StartWorkflowWithVars returns once it
			// parks here instead of running resume_original nested inside its
			// own "starting" marker, exactly like the real "fix" run_agent
			// step does in production.
			{
				ID:   "await_fix",
				Name: "Await Fix",
				Type: workflow.StepWaitHuman,
				Config: workflow.StepConfig{
					HumanActions: []string{"done"},
				},
				Next: []workflow.Transition{{GoTo: "resume_original"}},
			},
			{
				ID:   "resume_original",
				Name: "Resume Interrupted Workflow",
				Type: workflow.StepResumeWorkflow,
				Next: []workflow.Transition{{GoTo: ""}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Mechanical stand-in resume target (no run_agent step, same rationale as
	// above): lets the happy-path test assert that resume_workflow actually
	// re-entered and ran the captured workflow, deterministically and without
	// spawning a real agent.
	if err := wfStore.Save(workflow.Definition{
		ID:   "resume-target-test",
		Name: "Resume target (test stand-in)",
		Trigger: workflow.Trigger{On: "task.status_changed", Conditions: []workflow.Condition{{
			Field: "task.tags", Operator: "contains", Value: "__never_dispatch_resume_target_test__",
		}}},
		Steps: []workflow.Step{{
			ID:   "mark_resumed",
			Name: "Mark Resumed",
			Type: workflow.StepSetStatus,
			Config: workflow.StepConfig{
				Status: "in-review",
			},
			Next: []workflow.Transition{{GoTo: ""}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	engine := workflow.NewEngine(wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agents, tasks: tasks},
		logger,
	)
	r := &Handler{
		logger: slog.New(slog.DiscardHandler), emit: func(string, any) {},
		tasks:     tasks,
		projects:  projects,
		agents:    agents,
		prTracker: github.NewIssueTracker(0),
		worktrees: worktree.New(worktree.Config{
			WorktreesDir: filepath.Join(tmp, "worktrees"),
			Projects:     projects,
			Tasks:        tasks,
			Logger:       slog.New(slog.DiscardHandler),
		}),
		WorkflowEngine: engine,
		wtFailures:     make(map[string]int),
	}

	tk, err := tasks.Create("no pr rebase recover", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	tk, err = tasks.Update(tk.ID, task.Update{
		ProjectID: task.Ptr("owner/repo"),
		Branch:    task.Ptr("feature/no-pr-recover"),
		Status:    task.Ptr(initialStatus),
		Workflow:  &priorWorkflow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, tk
}

// TestRecoverBranchConflictNoPR_ReturnsTrueAndDispatchesRecoveryWorkflow
// verifies the happy path: a task with no PR yet, whose branch already
// exists, gets its worktree prepared via PrepareForBranchFix, the recovery
// workflow is dispatched (never jumped straight to a terminal status), the
// captured interrupted workflow is resumed by resume_workflow (not skipped),
// and the conflict-issue retry budget is marked handled so the same
// conflict doesn't immediately re-trigger.
func TestRecoverBranchConflictNoPR_ReturnsTrueAndDispatchesRecoveryWorkflow(t *testing.T) {
	priorWorkflow := &workflow.Execution{
		WorkflowID:  "resume-target-test",
		CurrentStep: "mark_resumed",
		State:       workflow.ExecRunning,
		Variables:   map[string]string{"attempt": "1"},
	}
	r, tk := setupBranchConflictNoPRHandler(t, task.StatusTesting, priorWorkflow)
	tk, err := r.tasks.Update(tk.ID, task.Update{StatusReason: task.Ptr("paused for testing before recovery")})
	if err != nil {
		t.Fatal(err)
	}

	if !r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("RecoverStaleBranchConflict returned false for a no-PR task with a real branch")
	}

	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == task.StatusHumanRequired {
		t.Fatalf("status = %q, want dispatched recovery instead of human-required", got.Status)
	}
	// set_recovering (sync) ran and the workflow parked at the async
	// await_fix step (the stand-in for the real workflow's run_agent "fix"
	// step) — the same shape as production, where resume_workflow only ever
	// runs after that async step's own completion drives AdvanceStep forward.
	if got.Workflow == nil || got.Workflow.WorkflowID != branchConflictFixTestWorkflowID {
		t.Fatalf("workflow = %+v, want %s dispatched", got.Workflow, branchConflictFixTestWorkflowID)
	}
	if got.Workflow.CurrentStep != "await_fix" {
		t.Fatalf("current step = %q, want await_fix", got.Workflow.CurrentStep)
	}
	// The captured interrupted workflow (ID + a copy of its vars) and the
	// task's pre-recovery status travel with the recovery execution so
	// resume_workflow can re-enter the exact interrupted stage once the fix
	// lands — proven directly (without a real agent) by
	// TestExecResumeWorkflow_ResumesCapturedTarget in the workflow package.
	if resumeID := got.Workflow.Variables["resume_workflow_id"]; resumeID != "resume-target-test" {
		t.Fatalf("resume_workflow_id = %q, want captured resume-target-test", resumeID)
	}
	if resumeStep := got.Workflow.Variables["resume_workflow_step"]; resumeStep != "mark_resumed" {
		t.Fatalf("resume_workflow_step = %q, want captured mark_resumed", resumeStep)
	}
	if resumeStatus := got.Workflow.Variables["resume_status"]; resumeStatus != string(task.StatusTesting) {
		t.Fatalf("resume_status = %q, want captured %q", resumeStatus, task.StatusTesting)
	}
	if resumeStatusReason := got.Workflow.Variables["resume_status_reason"]; resumeStatusReason != "paused for testing before recovery" {
		t.Fatalf("resume_status_reason = %q, want captured pre-recovery reason", resumeStatusReason)
	}
	if resumeVars := got.Workflow.Variables["resume_workflow_vars"]; resumeVars != `{"attempt":"1"}` {
		t.Fatalf("resume_workflow_vars = %q, want captured prior workflow vars", resumeVars)
	}
	if r.prTracker.Retries(tk.ID, github.PRIssueBranchConflictNoPR) == 0 {
		t.Fatal("branch-conflict retry budget was not marked handled after recovery dispatch")
	}
	if r.prTracker.Retries(tk.ID, github.PRIssueConflict) != 0 {
		t.Fatal("PR conflict retry budget should stay untouched for no-PR recovery")
	}
}

// TestRecoverBranchConflictNoPR_AtCapEscalates verifies the retry-budget
// guard: an exhausted branch-conflict-no-PR budget
// returns false so the caller escalates to human-required instead of
// retrying forever on a genuinely unresolvable conflict.
func TestRecoverBranchConflictNoPR_AtCapEscalates(t *testing.T) {
	r, tk := setupBranchConflictNoPRHandler(t, task.StatusInProgress, nil)
	for range github.MaxRetries {
		r.prTracker.MarkHandled(tk.ID, github.PRIssueBranchConflictNoPR, "somesha")
		r.prTracker.MarkHandled(tk.ID, branchRecreateKind, "")
	}

	if r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("RecoverStaleBranchConflict returned true despite exhausted retry budget")
	}
	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatalf("workflow = %+v, want no recovery workflow dispatched", got.Workflow)
	}
	// markConflictRecoveryExhausted must park the task itself with an
	// attempt-count reason (task bdcc90a4) so an operator — or the automated
	// human-review agent — can tell an exhausted recovery loop apart from a
	// fresh, first-time conflict instead of just seeing the generic
	// worktreeerr.RebaseBlockedReason.
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	wantReason := fmt.Sprintf("branch conflict recovery attempted %d time(s) and failed", github.MaxRetries)
	if !strings.Contains(got.StatusReason, wantReason) {
		t.Fatalf("status reason = %q, want it to contain %q", got.StatusReason, wantReason)
	}
}

// TestRecoverBranchConflictNoPR_MissingBranchEscalates verifies that a task
// whose branch does not exist (PrepareForBranchFix returns
// worktree.ErrTaskBranchMissing) fails closed rather than guessing at a ref.
func TestRecoverBranchConflictNoPR_MissingBranchEscalates(t *testing.T) {
	r, tk := setupBranchConflictNoPRHandler(t, task.StatusInProgress, nil)
	if _, err := r.tasks.Update(tk.ID, task.Update{Branch: task.Ptr("branch/does-not-exist")}); err != nil {
		t.Fatal(err)
	}

	for i := range wtFailureLimit - 1 {
		if !r.RecoverStaleBranchConflict(tk.ID) {
			t.Fatalf("call %d: want true (parked for retry) before the circuit trips", i+1)
		}
		got, err := r.tasks.Get(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == task.StatusHumanRequired {
			t.Fatalf("call %d: escalated before the circuit limit", i+1)
		}
	}
	if r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("circuit-limit call: want false")
	}
	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required after the circuit trips", got.Status)
	}
}

// TestRecoverBranchConflictNoPR_InFlightGuardPreventsDoubleDispatch verifies
// that a second recovery attempt for a task already mid-recovery bails out
// instead of starting a duplicate worktree prep / workflow dispatch.
func TestRecoverBranchConflictNoPR_InFlightGuardPreventsDoubleDispatch(t *testing.T) {
	r, tk := setupBranchConflictNoPRHandler(t, task.StatusInProgress, nil)
	r.branchRecoveryMu.Lock()
	r.branchRecoveryInFlight = map[string]struct{}{tk.ID: {}}
	r.branchRecoveryMu.Unlock()

	if r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("RecoverStaleBranchConflict returned true while a recovery was already marked in-flight")
	}
	if r.prTracker.Retries(tk.ID, github.PRIssueBranchConflictNoPR) != 0 {
		t.Fatal("branch-conflict retry budget was marked handled despite the in-flight guard rejecting the attempt")
	}
}

func TestRecoverBranchConflictNoPR_DispatchFailureRestoresPriorWorkflow(t *testing.T) {
	priorWorkflow := &workflow.Execution{
		WorkflowID:  "resume-target-test",
		CurrentStep: "mark_resumed",
		State:       workflow.ExecRunning,
		Variables:   map[string]string{"attempt": "2"},
	}
	r, tk := setupBranchConflictNoPRHandler(t, task.StatusTesting, priorWorkflow)
	emptyStore, err := workflow.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.WorkflowEngine = workflow.NewEngine(emptyStore, &taskAdapter{tasks: r.tasks}, &agentAdapter{agents: r.agents, tasks: r.tasks}, slog.New(slog.DiscardHandler))

	if r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("recoverBranchConflictNoPR returned true despite dispatch failure")
	}

	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusTesting {
		t.Fatalf("status = %q, want restored %q", got.Status, task.StatusTesting)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != priorWorkflow.WorkflowID {
		t.Fatalf("workflow = %+v, want restored prior workflow", got.Workflow)
	}
	if got.Workflow.CurrentStep != priorWorkflow.CurrentStep {
		t.Fatalf("current step = %q, want restored %q", got.Workflow.CurrentStep, priorWorkflow.CurrentStep)
	}
	if got.Workflow.State != priorWorkflow.State {
		t.Fatalf("state = %q, want restored %q", got.Workflow.State, priorWorkflow.State)
	}
}

// blockingAgentLauncher implements workflow.AgentLauncher with a StartAgent
// that signals entryCh once invoked and then blocks until releaseCh closes.
// Used to genuinely hold the engine's per-task starting marker open for the
// duration of a concurrent dispatchBranchConflictRecovery attempt — mirroring
// the real race (restart-stale's raw StartWorkflow(implement) still deep
// inside its own worktree-prep/StartAgent call when a second recovery path's
// StartWorkflowWithVars reaches the same task).
type blockingAgentLauncher struct {
	entryCh   chan struct{}
	releaseCh chan struct{}
}

func (b *blockingAgentLauncher) StartAgent(string, string, string, string, string, string, string, []string, bool, bool, string, string, workflow.AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	close(b.entryCh)
	<-b.releaseCh
	return "", "", "", workflow.ErrDispatchInFlight
}
func (b *blockingAgentLauncher) HasRunningAgent(string) bool                     { return false }
func (b *blockingAgentLauncher) HasOtherRunningAgentForTask(string, string) bool { return false }
func (b *blockingAgentLauncher) FindRunningAgentForRole(string, string) (string, bool) {
	return "", false
}
func (b *blockingAgentLauncher) StopAgentsForTask(string, string) {}
func (b *blockingAgentLauncher) SendPrompt(string, string) error  { return nil }
func (b *blockingAgentLauncher) DefaultProvider() string          { return "claude" }
func (b *blockingAgentLauncher) ProviderRateLimited(string) bool  { return false }
func (b *blockingAgentLauncher) ProviderCanFailover(string) bool  { return false }
func (b *blockingAgentLauncher) ProviderHealthy(string) bool      { return true }
func (b *blockingAgentLauncher) IsDispatching(string) bool        { return false }

// TestDispatchBranchConflictRecovery_QueuesRetryInsteadOfGivingUpWhenMarkerHeld
// locks the fix for dispatchBranchConflictRecovery's own re-dispatch call: a
// concurrent StartWorkflow call for the same task can grab the engine's
// starting marker sometime during the caller's own (multi-second)
// PrepareForBranchFix/worktree-setup work — a TOCTOU window
// TryConflictRecovery's upfront check alone cannot close. When that happens,
// StartWorkflowWithVars fails with ErrWorkflowAlreadyActive; the caller must
// queue a retry (QueueConflictRecoveryRetry) instead of restoring the prior
// workflow and giving up, so the recovery actually runs once the marker
// releases rather than stranding the task on whatever the concurrent call's
// own error path decides (e.g. an unaided rebase failure escalating straight
// to human-required).
func TestDispatchBranchConflictRecovery_QueuesRetryInsteadOfGivingUpWhenMarkerHeld(t *testing.T) {
	dir, err := os.MkdirTemp("", "sybra-test-tasks-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)

	tk, err := tasks.Create("blocked dispatch", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	tk, err = tasks.Update(tk.ID, task.Update{
		ProjectID: task.Ptr("owner/repo"),
		Status:    task.Ptr(task.StatusInProgress),
	})
	if err != nil {
		t.Fatal(err)
	}

	wfStore, err := workflow.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const blockerWorkflowID = "blocker-test"
	if err := wfStore.Save(workflow.Definition{
		ID:   blockerWorkflowID,
		Name: "Blocker (test stand-in)",
		Trigger: workflow.Trigger{On: "task.status_changed", Conditions: []workflow.Condition{{
			Field: "task.tags", Operator: "contains", Value: "__never_dispatch_blocker_test__",
		}}},
		Steps: []workflow.Step{{
			ID:     "block",
			Name:   "Block",
			Type:   workflow.StepRunAgent,
			Config: workflow.StepConfig{Role: "implementation"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := wfStore.Save(workflow.Definition{
		ID:   branchConflictFixTestWorkflowID,
		Name: "Branch conflict fix (test stand-in)",
		Trigger: workflow.Trigger{On: "task.status_changed", Conditions: []workflow.Condition{{
			Field: "task.tags", Operator: "contains", Value: "__never_dispatch_branch_conflict_fix_test2__",
		}}},
		Steps: []workflow.Step{{
			ID:     "set_recovering",
			Name:   "Mark Recovering",
			Type:   workflow.StepSetStatus,
			Config: workflow.StepConfig{Status: "in-progress", StatusReason: "recovering from a branch conflict (no PR yet)"},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	launcher := &blockingAgentLauncher{entryCh: make(chan struct{}), releaseCh: make(chan struct{})}
	engine := workflow.NewEngine(wfStore, &taskAdapter{tasks: tasks}, launcher, logger)
	r := &Handler{
		logger: logger, emit: func(string, any) {},
		tasks: tasks, prTracker: github.NewIssueTracker(0),
		WorkflowEngine: engine, wtFailures: make(map[string]int),
	}
	// The real recovery callback re-derives fresh state and re-dispatches —
	// stood in here without RecoverStaleBranchConflict's PR-fetch/worktree-prep
	// plumbing, since this test is only proving the queue-and-drain round trip.
	engine.SetConflictRecovery(func(taskID string) bool {
		fresh, gerr := tasks.Get(taskID)
		if gerr != nil {
			return false
		}
		resume := r.captureBranchConflictResumeState(fresh)
		// Mirrors RecoverStaleBranchConflict/recoverBranchConflictNoPR: clear
		// the still-attached (now-terminal-in-practice, parked) prior workflow
		// record before re-dispatching, same as production does before ever
		// reaching dispatchBranchConflictRecovery.
		if engine.HasActiveWorkflow(taskID) {
			if _, cErr := engine.CancelWorkflow(taskID, "test conflict recovery"); cErr != nil {
				return false
			}
		}
		return r.dispatchBranchConflictRecovery(taskID, "/fake/dir", "main", fresh, "", resume, false)
	})

	done := make(chan error, 1)
	go func() { done <- engine.StartWorkflow(tk.ID, blockerWorkflowID) }()

	select {
	case <-launcher.entryCh:
	case <-time.After(5 * time.Second):
		t.Fatal("blocker workflow never entered StartAgent")
	}

	fresh, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	resume := r.captureBranchConflictResumeState(fresh)
	if !r.dispatchBranchConflictRecovery(tk.ID, "/fake/dir", "main", fresh, "", resume, false) {
		t.Fatal("dispatchBranchConflictRecovery = false while marker held, want true (queued for retry)")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != blockerWorkflowID {
		t.Fatalf("workflow = %+v, want unchanged %s (no restore while a retry is queued)", got.Workflow, blockerWorkflowID)
	}

	close(launcher.releaseCh)
	if err := <-done; err != nil {
		t.Fatalf("blocker workflow start: %v", err)
	}

	got, err = tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != branchConflictFixTestWorkflowID {
		t.Fatalf("workflow after drain = %+v, want %s dispatched by the drained retry", got.Workflow, branchConflictFixTestWorkflowID)
	}
}

func TestRecoverBranchConflictNoPR_WorktreeFailuresTripCircuitBreaker(t *testing.T) {
	r, tk := setupBranchConflictNoPRHandler(t, task.StatusInProgress, nil)
	if _, err := r.tasks.Update(tk.ID, task.Update{Branch: task.Ptr("main")}); err != nil {
		t.Fatal(err)
	}

	for i := range wtFailureLimit - 1 {
		if !r.RecoverStaleBranchConflict(tk.ID) {
			t.Fatalf("call %d: want true (parked for retry) on transient worktree failure", i+1)
		}
		got, err := r.tasks.Get(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == task.StatusHumanRequired {
			t.Fatalf("call %d: escalated too early", i+1)
		}
	}

	if r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("circuit-open call: want false")
	}
	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q after circuit break, want human-required", got.Status)
	}
	if n := r.wtFailures[tk.ID]; n != 0 {
		t.Fatalf("wtFailures[%s] = %d after circuit trip, want 0", tk.ID, n)
	}
}

func TestRecoverBranchConflictNoPR_RecreatesWhenExhausted(t *testing.T) {
	r, tk := setupBranchConflictNoPRHandler(t, task.StatusInProgress, nil)
	for range github.MaxRetries {
		r.prTracker.MarkHandled(tk.ID, branchConflictRetryKind, "")
	}
	if !r.prTracker.AtCap(tk.ID, branchConflictRetryKind) {
		t.Fatal("precondition: conflict-recovery budget should be at cap")
	}

	if !r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("want true: exhausted conflict recovery should recreate the branch, not escalate")
	}
	got, err := r.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress (recreated, re-implementing)", got.Status)
	}
	if n := r.prTracker.Retries(tk.ID, branchConflictRetryKind); n != 0 {
		t.Errorf("conflict-recovery budget = %d, want 0 (reset for the fresh branch)", n)
	}
	if n := r.prTracker.Retries(tk.ID, branchRecreateKind); n != 1 {
		t.Errorf("recreate counter = %d, want 1", n)
	}
}

func TestRecoverBranchConflictNoPR_RecreateBudgetExhaustedEscalates(t *testing.T) {
	r, tk := setupBranchConflictNoPRHandler(t, task.StatusInProgress, nil)
	for range github.MaxRetries {
		r.prTracker.MarkHandled(tk.ID, branchConflictRetryKind, "")
		r.prTracker.MarkHandled(tk.ID, branchRecreateKind, "")
	}

	if r.RecoverStaleBranchConflict(tk.ID) {
		t.Fatal("want false: both conflict-recovery and recreate budgets exhausted must escalate")
	}
	if got, _ := r.tasks.Get(tk.ID); got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
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
