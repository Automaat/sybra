package reconciliation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/reconcile"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestCanCleanupObservesTheExactRequestedPath(t *testing.T) {
	t.Parallel()
	canonical := t.TempDir()
	attempt := t.TempDir()
	for _, repo := range []string{canonical, attempt} {
		runGit(t, repo, "init", "-b", "main")
		runGit(t, repo, "config", "user.name", "Sybra Test")
		runGit(t, repo, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "add", "base.txt")
		runGit(t, repo, "commit", "-m", "base")
	}
	if err := os.WriteFile(filepath.Join(attempt, "uncommitted.txt"), []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	created, err := tasks.CreateFull("cleanup", "", task.AgentModeHeadless, task.Update{WorktreeDir: task.Ptr(canonical)})
	if err != nil {
		t.Fatal(err)
	}
	runner := New(Config{Tasks: tasks})
	if runner.CanCleanup(context.Background(), created, attempt) {
		t.Fatal("cleanup gate approved a dirty requested attempt path by observing the clean canonical path")
	}
}

func TestReconcileTreatsPrunedWorkflowRouteAsStaleLease(t *testing.T) {
	t.Parallel()
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	wf := &workflow.Execution{WorkflowID: "wf", CurrentStep: "current", AgentRoutes: map[string]string{"new-run": "current"}}
	created, err := tasks.Create("lease", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tasks.Update(created.ID, task.Update{Workflow: task.Ptr(wf)}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(created.ID, task.AgentRun{AgentID: "old-run", Role: "implementation", Mode: "headless", State: "stopped", Outcome: task.RunOutcomeSuccess}); err != nil {
		t.Fatal(err)
	}
	plan, err := New(Config{Tasks: tasks}).Reconcile(context.Background(), reconcile.Request{TaskID: created.ID, RunID: "old-run", Intent: reconcile.IntentAuthorCompletion})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != reconcile.ActionWait || plan.Reason != "stale or missing attempt lease" {
		t.Fatalf("pruned route plan = %#v", plan)
	}
}

func TestReconcileCheckpointsDirtyAuthorWorkBeforeAdvancing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "config", "user.name", "Sybra Test")
	runGit(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "base")

	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	created, err := tasks.CreateFull("checkpoint", "", task.AgentModeHeadless, task.Update{WorktreeDir: task.Ptr(repo)})
	if err != nil {
		t.Fatal(err)
	}
	const runID = "author-1"
	if err := tasks.AddRun(created.ID, task.AgentRun{AgentID: runID, Role: "implementation", Mode: "headless", State: "stopped", Outcome: task.RunOutcomeSuccess}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "work.txt"), []byte("preserve me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := New(Config{Tasks: tasks})
	plan, err := runner.Reconcile(ctx, reconcile.Request{TaskID: created.ID, RunID: runID, Intent: reconcile.IntentAuthorCompletion})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if plan.Action != reconcile.ActionAdvance {
		t.Fatalf("action = %q, want advance", plan.Action)
	}
	if got := runGit(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("worktree remained dirty after checkpoint: %q", got)
	}
	if got := runGit(t, repo, "show", "--format=", "--name-only", "HEAD"); got != "work.txt\n" {
		t.Fatalf("checkpoint files = %q, want work.txt", got)
	}
}

func TestObservedSidecarsRecordsOnlyContentDigests(t *testing.T) {
	t.Parallel()
	taskState := task.Task{
		Plan:       "private plan content",
		CodeReview: "private review content",
		PlanDrafts: map[string]string{providerid.Codex: "private draft", "empty": ""},
	}
	got := observedSidecars(taskState)
	if len(got) != 3 {
		t.Fatalf("observed sidecars = %#v, want three non-empty entries", got)
	}
	for _, item := range got {
		if item.Digest == "" || item.Digest == taskState.Plan || item.Digest == taskState.CodeReview {
			t.Fatalf("sidecar %q did not retain a content-only digest: %#v", item.Name, item)
		}
	}
}

func TestObservedEvidenceKeepsRevisionBinding(t *testing.T) {
	t.Parallel()
	got := observedEvidence(evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: "verify_checks", ExitStatus: 0, FinalRev: "head", Timestamp: time.Now()},
		{Criterion: "review", ExitStatus: 0, FinalRev: "head", Timestamp: time.Now()},
	}})
	if !got.Verified || got.SourceSHA != "head" || len(got.Items) != 2 {
		t.Fatalf("observed evidence = %#v", got)
	}
	got = observedEvidence(evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: "verify_checks", ExitStatus: 0, FinalRev: "old"},
		{Criterion: "review", ExitStatus: 0, FinalRev: "head"},
	}})
	if got.Verified || got.SourceSHA != "" {
		t.Fatalf("mixed-source evidence was accepted: %#v", got)
	}
}
