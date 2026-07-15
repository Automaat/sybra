package review

import (
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

func newTasklessAdoptHandler(t *testing.T) (*Handler, *task.Manager) {
	t.Helper()
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	return &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		prTracker: github.NewIssueTracker(time.Minute),
	}, tasks
}

func TestAdoptTasklessPRs_ResurrectsSybraPROnly(t *testing.T) {
	r, tasks := newTasklessAdoptHandler(t)
	triaged := make(chan task.Task, 1)
	r.triageReviewFn = func(t task.Task) { triaged <- t }
	prs := []github.PullRequest{
		{Number: 1677, Repository: "owner/repo", HeadRefName: "perf/bound-limits-1a2b3c4d", Title: "perf: bound", URL: "https://x/1677"},
		{Number: 99, Repository: "owner/repo", HeadRefName: "external/random-branch", Title: "ext", URL: "https://x/99"},
		{Number: 42, Repository: "owner/repo", HeadRefName: "fix/thing-deadbeef", Title: "draft", URL: "https://x/42", IsDraft: true},
	}

	r.adoptTasklessPRs(nil, prs)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("created %d tasks, want 1 (only the non-draft Sybra-branch PR)", len(all))
	}
	got := all[0]
	if got.PRNumber != 1677 || got.Branch != "perf/bound-limits-1a2b3c4d" {
		t.Fatalf("adopted task = {pr:%d status:%q branch:%q}, want {1677 * perf/bound-limits-1a2b3c4d}", got.PRNumber, got.Status, got.Branch)
	}
	if !slices.Contains(got.Tags, "review") {
		t.Error("adopted task missing the review tag — would let simple-task-plan claim task.created")
	}

	select {
	case triagedTask := <-triaged:
		if triagedTask.ID != got.ID {
			t.Fatalf("triaged task ID = %q, want %q", triagedTask.ID, got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("triageReview was not dispatched for the taskless-adopted task")
	}
}

func TestAdoptTasklessPRs_SkipsAlreadyTracked(t *testing.T) {
	r, tasks := newTasklessAdoptHandler(t)
	existing := []task.Task{
		{ID: "t1", ProjectID: "owner/repo", PRNumber: 1677, Branch: "perf/bound-limits-1a2b3c4d"},
		{ID: "t2", ProjectID: "owner/repo", Branch: "fix/other-cafe1234"},
	}
	prs := []github.PullRequest{
		{Number: 1677, Repository: "owner/repo", HeadRefName: "perf/bound-limits-1a2b3c4d", Title: "already tracked by pr#", URL: "https://x/1677"},
		{Number: 500, Repository: "owner/repo", HeadRefName: "fix/other-cafe1234", Title: "already tracked by branch", URL: "https://x/500"},
	}

	r.adoptTasklessPRs(existing, prs)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("created %d tasks, want 0 (both PRs already tracked)", len(all))
	}
}

func TestAdoptTasklessPRs_SkipsOwnedBranchWithStaleProjectID(t *testing.T) {
	r, tasks := newTasklessAdoptHandler(t)
	// Task's ProjectID has gone stale relative to the live PR's repository
	// (e.g. reassigned/mirrored elsewhere), so neither the ProjectID#PR nor
	// ProjectID|Branch keys match — only the branch's task-ID suffix does.
	existing := []task.Task{
		{ID: "696bc049", ProjectID: "owner/stale-repo", PRNumber: 1869, Branch: "fix/fix-ingest-ensure-per-task-unique-696bc049"},
	}
	prs := []github.PullRequest{
		{Number: 1869, Repository: "owner/repo", HeadRefName: "fix/fix-ingest-ensure-per-task-unique-696bc049", Title: "owned elsewhere", URL: "https://x/1869"},
	}

	r.adoptTasklessPRs(existing, prs)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("created %d tasks, want 0 (branch already owned by task 696bc049 despite stale ProjectID)", len(all))
	}
}
