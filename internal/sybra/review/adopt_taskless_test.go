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
	if got.PRNumber != 1677 || got.Status != task.StatusInReview || got.Branch != "perf/bound-limits-1a2b3c4d" {
		t.Fatalf("adopted task = {pr:%d status:%q branch:%q}, want {1677 in-review perf/bound-limits-1a2b3c4d}", got.PRNumber, got.Status, got.Branch)
	}
	if !slices.Contains(got.Tags, "review") {
		t.Error("adopted task missing the review tag — would let simple-task-plan claim task.created")
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
