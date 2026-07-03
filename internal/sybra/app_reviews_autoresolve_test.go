package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// initAutoResolveSourceRepo builds a minimal real git repo with one commit,
// used as the origin behind a bare clone so PrepareForFix has real refs to
// check out and autoResolveConflict has a real `git fetch origin <base>` to
// run against (the merge itself is stubbed via tryCleanMergeFn).
func initAutoResolveSourceRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runs := [][]string{
		{"init", dir},
		{"-C", dir, "config", "user.email", "test@test.com"},
		{"-C", dir, "config", "user.name", "Test"},
		{"-C", dir, "config", "commit.gpgsign", "false"},
	}
	for _, args := range runs {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", dir, "add", "."}, {"-C", dir, "commit", "-m", "init"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// autoResolveHarness wires a ReviewHandler backed by a real project/worktree
// stack and a mechanical pr-fix workflow (mechanicalPRFixYAML, defined in
// app_reviews_coalesce_test.go) that completes without spawning an agent —
// enough to distinguish "the fast-path skipped the agent" from "the normal
// pr-fix workflow was dispatched" without needing a real claude/codex binary.
type autoResolveHarness struct {
	r        *ReviewHandler
	tasks    *task.Manager
	projects *project.Store
	auditDir string
	proj     project.Project
	branch   string
}

func newAutoResolveHarness(t *testing.T, autoResolveEnabled bool) *autoResolveHarness {
	t.Helper()
	tmp := t.TempDir()
	logger := slog.New(slog.DiscardHandler)

	taskStore, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(taskStore, nil)

	wfStore, err := workflow.NewStore(filepath.Join(tmp, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfStore.Dir(), "test-pr-fix.yaml"), []byte(mechanicalPRFixYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, filepath.Join(tmp, "logs"))
	engine := workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agentMgr, tasks: tasks},
		logger,
	)

	src := initAutoResolveSourceRepo(t)
	bare := filepath.Join(tmp, "clones", "o", "r.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	branch, err := project.DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	projDir := filepath.Join(tmp, "projects")
	clonesDir := filepath.Join(tmp, "clones")
	projStore, err := project.NewStore(projDir, clonesDir)
	if err != nil {
		t.Fatal(err)
	}
	proj := project.Project{
		ID: "o/r", Name: "r", Owner: "o", Repo: "r",
		URL: src, ClonePath: bare, Type: project.ProjectTypePet,
	}
	seedExperienceProject(t, projDir, proj)

	wt := worktree.New(worktree.Config{
		WorktreesDir: filepath.Join(tmp, "worktrees"),
		Projects:     projStore,
		Tasks:        tasks,
		Logger:       logger,
		LogsDir:      filepath.Join(tmp, "wt-logs"),
		PRBranchResolver: func(string, int) (string, error) {
			return branch, nil
		},
		AgentChecker: agentMgr.HasRunningAgentForTask,
	})

	auditDir := filepath.Join(tmp, "audit")
	al, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}

	r := &ReviewHandler{
		DomainHandler:  DomainHandler{logger: logger, audit: al, emit: func(string, any) {}},
		tasks:          tasks,
		projects:       projStore,
		agents:         agentMgr,
		prTracker:      github.NewIssueTracker(time.Minute),
		worktrees:      wt,
		workflowEngine: engine,
		cfg: &config.Config{GitHub: config.GitHubConfig{
			AutoResolveCleanMerges: autoResolveEnabled,
		}},
	}
	return &autoResolveHarness{
		r:        r,
		tasks:    tasks,
		projects: projStore,
		auditDir: auditDir,
		proj:     proj,
		branch:   branch,
	}
}

func (h *autoResolveHarness) newConflictTask(t *testing.T) (task.Task, github.PullRequest) {
	t.Helper()
	tk, err := h.tasks.Create("conflict pr", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Update(tk.ID, task.Update{
		ProjectID: task.Ptr(h.proj.ID),
		Status:    task.Ptr(task.StatusInReview),
		PRNumber:  task.Ptr(555),
	})
	if err != nil {
		t.Fatal(err)
	}
	pr := github.PullRequest{
		Number: 555, Repository: h.proj.ID, HeadRefName: h.branch,
		BaseRefName: h.branch, HeadSHA: "sha-1",
	}
	return tk, pr
}

func stubMerge(result project.CleanMergeResult, err error) func(context.Context, string, string) (project.CleanMergeResult, error) {
	return func(context.Context, string, string) (project.CleanMergeResult, error) {
		return result, err
	}
}

func stubPush(err error) func(context.Context, string, string) error {
	return func(context.Context, string, string) error { return err }
}

func currentHEAD(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestAutoResolveConflict_CreatedSkipsAgent is the acceptance criterion at the
// center of #1439: a clean merge that produces a new commit is pushed and no
// pr-fix agent is spawned.
func TestAutoResolveConflict_CreatedSkipsAgent(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	var mergedHead string
	h.r.tryCleanMergeFn = func(_ context.Context, dir, _ string) (project.CleanMergeResult, error) {
		cmds := [][]string{
			{"-C", dir, "config", "user.email", "test@test.com"},
			{"-C", dir, "config", "user.name", "Test"},
			{"-C", dir, "commit", "--allow-empty", "-m", "synthetic merge"},
		}
		for _, args := range cmds {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
		mergedHead = currentHEAD(t, dir)
		return project.CleanMergeCreated, nil
	}
	h.r.pushSyncFn = stubPush(nil)

	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true for a created clean merge")
	}

	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Errorf("workflow dispatched = %+v, want nil (agent must not run)", got.Workflow)
	}
	if mergedHead == "" {
		t.Fatal("mergedHead not captured")
	}
	if h.r.prTracker.ShouldHandle(tk.ID, github.PRIssueConflict, mergedHead) {
		t.Error("conflict issue not marked handled for the pushed merge commit")
	}

	events := readExperienceAuditEvents(t, h.auditDir)
	found := false
	for _, ev := range events {
		if ev.Type != audit.EventPRConflictAutoResolved {
			continue
		}
		found = true
		if ev.Data["pr"] != float64(555) {
			t.Errorf("audit pr = %v, want 555", ev.Data["pr"])
		}
		if ev.Data["issue"] != string(github.PRIssueConflict) {
			t.Errorf("audit issue = %v, want %v", ev.Data["issue"], github.PRIssueConflict)
		}
		for k := range ev.Data {
			if k != "pr" && k != "issue" {
				t.Errorf("audit payload carries unexpected field %q: %v", k, ev.Data)
			}
		}
	}
	if !found {
		t.Error("EventPRConflictAutoResolved not emitted")
	}
}

// TestAutoResolveConflict_ConflictFallsThroughToAgent covers a merge that
// genuinely reports conflicting hunks: the fast-path must not mark anything
// handled and the normal pr-fix workflow must still be dispatched.
func TestAutoResolveConflict_ConflictFallsThroughToAgent(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	h.r.tryCleanMergeFn = stubMerge(project.CleanMergeConflict, nil)
	h.r.pushSyncFn = stubPush(nil)

	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true (agent dispatched via mechanical workflow)")
	}

	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched for a conflicting merge")
	}
	if k := got.Workflow.Variables["pr_issue_kind"]; k != string(github.PRIssueConflict) {
		t.Errorf("pr_issue_kind = %q, want %q", k, github.PRIssueConflict)
	}
	for _, ev := range readExperienceAuditEvents(t, h.auditDir) {
		if ev.Type == audit.EventPRConflictAutoResolved {
			t.Error("EventPRConflictAutoResolved emitted for a conflicting merge")
		}
	}
}

// TestAutoResolveConflict_NoopFallsThroughToAgent covers the branch-already-
// contains-base case: nothing to push, so auto-resolve must not claim the
// issue as handled — the agent still needs to look at why GitHub reports a
// conflict despite a locally clean merge.
func TestAutoResolveConflict_NoopFallsThroughToAgent(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	h.r.tryCleanMergeFn = stubMerge(project.CleanMergeNoop, nil)
	h.r.pushSyncFn = stubPush(nil)

	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true (agent dispatched)")
	}

	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched for a no-op merge")
	}
	for _, ev := range readExperienceAuditEvents(t, h.auditDir) {
		if ev.Type == audit.EventPRConflictAutoResolved {
			t.Error("EventPRConflictAutoResolved emitted for a no-op merge")
		}
	}
}

// TestAutoResolveConflict_ActiveWorkflowSkipsFastPath is the mutation guard:
// when a workflow is already active for the task, dispatchFixIssues must
// return immediately without touching the worktree or the fast-path.
func TestAutoResolveConflict_ActiveWorkflowSkipsFastPath(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	active := &workflow.Execution{WorkflowID: "test-pr-fix", CurrentStep: "mark", State: workflow.ExecRunning}
	if _, err := h.tasks.Update(tk.ID, task.Update{Workflow: &active}); err != nil {
		t.Fatal(err)
	}
	mergeCalled := false
	h.r.tryCleanMergeFn = func(context.Context, string, string) (project.CleanMergeResult, error) {
		mergeCalled = true
		return project.CleanMergeCreated, nil
	}

	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})
	if ok {
		t.Error("dispatchFixIssues = true, want false while a workflow is already active")
	}
	if mergeCalled {
		t.Error("tryCleanMergeFn invoked while a workflow was already active")
	}
}

// TestAutoResolveConflict_CoalescedIssuesSkipFastPath verifies the fast-path
// only ever applies to a single, conflict-only issue: a coalesced
// conflict+comments dispatch must always go through the normal agent path,
// regardless of what the merge would have done.
func TestAutoResolveConflict_CoalescedIssuesSkipFastPath(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	mergeCalled := false
	h.r.tryCleanMergeFn = func(context.Context, string, string) (project.CleanMergeResult, error) {
		mergeCalled = true
		return project.CleanMergeCreated, nil
	}
	h.r.pushSyncFn = stubPush(nil)

	commentsPR := pr
	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
		{Kind: github.PRIssueComments, TaskID: tk.ID, PR: commentsPR},
	})
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true (agent dispatched)")
	}
	if mergeCalled {
		t.Error("tryCleanMergeFn invoked for a coalesced (non-single) issue set")
	}
	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched for the coalesced issue set")
	}
}

// TestAutoResolveConflict_ToggleOffSkipsFastPath verifies the config
// kill-switch: with AutoResolveCleanMerges left at its default false, a clean
// mergeable conflict still dispatches the normal agent path.
func TestAutoResolveConflict_ToggleOffSkipsFastPath(t *testing.T) {
	h := newAutoResolveHarness(t, false)
	tk, pr := h.newConflictTask(t)
	mergeCalled := false
	h.r.tryCleanMergeFn = func(context.Context, string, string) (project.CleanMergeResult, error) {
		mergeCalled = true
		return project.CleanMergeCreated, nil
	}
	h.r.pushSyncFn = stubPush(nil)

	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true (agent dispatched)")
	}
	if mergeCalled {
		t.Error("tryCleanMergeFn invoked while AutoResolveCleanMerges is disabled")
	}
	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched with the toggle off")
	}
}

func TestAutoResolveConflict_WorkProjectSkipsFastPath(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	if _, err := h.projects.Update(h.proj.ID, project.ProjectTypeWork); err != nil {
		t.Fatal(err)
	}

	tk, pr := h.newConflictTask(t)
	mergeCalled := false
	h.r.tryCleanMergeFn = func(context.Context, string, string) (project.CleanMergeResult, error) {
		mergeCalled = true
		return project.CleanMergeCreated, nil
	}
	h.r.pushSyncFn = stubPush(nil)

	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true (agent dispatched)")
	}
	if mergeCalled {
		t.Error("tryCleanMergeFn invoked for a work-typed project")
	}
	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched for a work-typed project")
	}
}

func TestAutoResolveConflict_InvalidBaseFallsThroughToAgent(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	pr.BaseRefName = "-not-a-branch"
	mergeCalled := false
	h.r.tryCleanMergeFn = func(context.Context, string, string) (project.CleanMergeResult, error) {
		mergeCalled = true
		return project.CleanMergeCreated, nil
	}
	h.r.pushSyncFn = stubPush(nil)

	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true (agent dispatched)")
	}
	if mergeCalled {
		t.Error("tryCleanMergeFn invoked for an invalid base ref")
	}
	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched for an invalid base ref")
	}
}

func TestAutoResolveConflict_PushFailureRollsBackMerge(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	worktreeDir := h.r.worktrees.PathFor(tk)
	var preMergeHead string
	h.r.tryCleanMergeFn = func(_ context.Context, dir, _ string) (project.CleanMergeResult, error) {
		preMergeHead = currentHEAD(t, dir)
		cmds := [][]string{
			{"-C", dir, "config", "user.email", "test@test.com"},
			{"-C", dir, "config", "user.name", "Test"},
			{"-C", dir, "commit", "--allow-empty", "-m", "synthetic merge"},
		}
		for _, args := range cmds {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
		return project.CleanMergeCreated, nil
	}
	h.r.pushSyncFn = stubPush(fmt.Errorf("push failed"))

	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true (agent dispatched after rollback)")
	}
	if preMergeHead == "" {
		t.Fatal("preMergeHead not captured")
	}
	if got := currentHEAD(t, worktreeDir); got != preMergeHead {
		t.Fatalf("HEAD after failed push = %s, want rollback to %s", got, preMergeHead)
	}
	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched after push failure")
	}
	for _, ev := range readExperienceAuditEvents(t, h.auditDir) {
		if ev.Type == audit.EventPRConflictAutoResolved {
			t.Fatal("EventPRConflictAutoResolved emitted after a failed push")
		}
	}
}

func TestAutoResolveConflict_EmptyBranchRollsBackMerge(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	worktreeDir, err := h.r.worktrees.PrepareForFix(context.Background(), tk, pr.Number)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}
	preMergeHead := currentHEAD(t, worktreeDir)

	if out, err := exec.Command("git", "-C", worktreeDir, "checkout", "--detach").CombinedOutput(); err != nil {
		t.Fatalf("git checkout --detach: %v: %s", err, out)
	}

	pushCalled := false
	h.r.tryCleanMergeFn = func(_ context.Context, dir, _ string) (project.CleanMergeResult, error) {
		cmds := [][]string{
			{"-C", dir, "config", "user.email", "test@test.com"},
			{"-C", dir, "config", "user.name", "Test"},
			{"-C", dir, "commit", "--allow-empty", "-m", "synthetic merge"},
		}
		for _, args := range cmds {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
		return project.CleanMergeCreated, nil
	}
	h.r.pushSyncFn = func(context.Context, string, string) error {
		pushCalled = true
		return nil
	}

	if ok := h.r.autoResolveConflict(context.Background(), tk, pr, worktreeDir); ok {
		t.Fatal("autoResolveConflict = true, want false for detached HEAD")
	}
	if pushCalled {
		t.Fatal("pushSyncFn called despite detached HEAD")
	}
	if got := currentHEAD(t, worktreeDir); got != preMergeHead {
		t.Fatalf("HEAD after empty-branch rollback = %s, want %s", got, preMergeHead)
	}
}

func TestRollbackAutoResolvedMerge_SurvivesCancelledContext(t *testing.T) {
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	worktreeDir, err := h.r.worktrees.PrepareForFix(context.Background(), tk, pr.Number)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}
	preMergeHead := currentHEAD(t, worktreeDir)

	cmds := [][]string{
		{"-C", worktreeDir, "config", "user.email", "test@test.com"},
		{"-C", worktreeDir, "config", "user.name", "Test"},
		{"-C", worktreeDir, "commit", "--allow-empty", "-m", "synthetic merge"},
	}
	for _, args := range cmds {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if got := currentHEAD(t, worktreeDir); got == preMergeHead {
		t.Fatal("expected synthetic merge commit before rollback")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.r.rollbackAutoResolvedMerge(ctx, tk.ID, pr.Number, worktreeDir, preMergeHead, "test")

	if got := currentHEAD(t, worktreeDir); got != preMergeHead {
		t.Fatalf("HEAD after rollback = %s, want %s", got, preMergeHead)
	}
}
