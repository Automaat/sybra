package sybra

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

// TestPRMonitorEligible exercises the scan predicate used by the PR monitor
// loop. The regression: tasks whose workflow exited to in-progress with a
// live PR number (because an evaluate step crashed, or a manually-spawned
// agent opened the PR outside the workflow) were silently dropped from the
// scan because it only considered status=in-review. Result: failing CI on
// those PRs was never fixed by pr-fix agents.
func TestPRMonitorEligible(t *testing.T) {
	tests := []struct {
		name string
		tk   task.Task
		want bool
	}{
		{
			name: "in-review with PR — original happy path",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42},
			want: true,
		},
		{
			name: "in-review with branch only — still eligible",
			tk:   task.Task{Status: task.StatusInReview, Branch: "sybra/feat-x"},
			want: true,
		},
		{
			name: "in-review with neither PR nor branch — not eligible",
			tk:   task.Task{Status: task.StatusInReview},
			want: false,
		},
		{
			name: "in-progress with PR — the regression case we're fixing",
			tk:   task.Task{Status: task.StatusInProgress, PRNumber: 247},
			want: true,
		},
		{
			name: "in-progress with branch only — not eligible (avoid WIP false positives)",
			tk:   task.Task{Status: task.StatusInProgress, Branch: "sybra/wip"},
			want: false,
		},
		{
			name: "in-progress with nothing — not eligible",
			tk:   task.Task{Status: task.StatusInProgress},
			want: false,
		},
		{
			name: "review tag excluded (inbound review task, not ours)",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42, Tags: []string{"review"}},
			want: false,
		},
		{
			name: "todo with PR — not eligible, not in monitored states",
			tk:   task.Task{Status: task.StatusTodo, PRNumber: 42},
			want: false,
		},
		{
			name: "done with PR — not eligible, already terminal",
			tk:   task.Task{Status: task.StatusDone, PRNumber: 42},
			want: false,
		},
		{
			name: "human-required with PR — not eligible, needs operator action first",
			tk:   task.Task{Status: task.StatusHumanRequired, PRNumber: 42},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prMonitorEligible(&tt.tk); got != tt.want {
				t.Errorf("prMonitorEligible(%+v) = %v, want %v", tt.tk, got, tt.want)
			}
		})
	}
}

func TestReviewClosedPREligible(t *testing.T) {
	tests := []struct {
		name string
		tk   task.Task
		want bool
	}{
		{
			name: "in-review review task with PR",
			tk: task.Task{
				Status:    task.StatusInReview,
				Tags:      []string{"review"},
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: true,
		},
		{
			name: "human-required review task with PR",
			tk: task.Task{
				Status:    task.StatusHumanRequired,
				Tags:      []string{"review"},
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: true,
		},
		{
			name: "done review task skipped",
			tk: task.Task{
				Status:    task.StatusDone,
				Tags:      []string{"review"},
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: false,
		},
		{
			name: "non-review task skipped",
			tk: task.Task{
				Status:    task.StatusInReview,
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: false,
		},
		{
			name: "review task without PR skipped",
			tk: task.Task{
				Status:    task.StatusInReview,
				Tags:      []string{"review"},
				ProjectID: "o/r",
			},
			want: false,
		},
		{
			name: "chat task skipped",
			tk: task.Task{
				Status:    task.StatusInReview,
				TaskType:  task.TaskTypeChat,
				Tags:      []string{"review"},
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewClosedPREligible(&tt.tk); got != tt.want {
				t.Errorf("reviewClosedPREligible(%+v) = %v, want %v", tt.tk, got, tt.want)
			}
		})
	}
}

func TestReviewTaskMatchers(t *testing.T) {
	tasks := []task.Task{
		{ID: "review", Status: task.StatusInReview, Tags: []string{"review"}, ProjectID: "o/r", PRNumber: 42},
		{ID: "done", Status: task.StatusDone, Tags: []string{"review"}, ProjectID: "o/r", PRNumber: 43},
		{ID: "mine", Status: task.StatusInReview, ProjectID: "o/r", PRNumber: 44},
	}

	got := reviewTaskMatchers(tasks)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if got[0].ID != "review" || got[0].ProjectID != "o/r" || got[0].PRNumber != 42 {
		t.Fatalf("matcher = %+v, want review o/r#42", got[0])
	}
}

func TestOpenReviewPRsIncludesApprovedReviews(t *testing.T) {
	requested := github.PullRequest{Number: 1, Repository: "o/r"}
	approved := github.PullRequest{Number: 2, Repository: "o/r", ViewerHasApproved: true}

	got := openReviewPRs(github.ReviewSummary{
		ReviewRequested: []github.PullRequest{requested},
		ReviewedByMe:    []github.PullRequest{approved},
	})

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("numbers = [%d %d], want [1 2]", got[0].Number, got[1].Number)
	}
}

func TestCreateReviewTaskPassesUpdatedTaskToTriage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	got := make(chan task.Task, 1)

	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		tasks:         tasks,
	}
	r.createReviewTaskWithTriage(github.PullRequest{
		Number: 2708,
		Title:  "docs: explain precedence",
		URL:    "https://github.com/kumahq/kuma-website/pull/2708",
		Author: "slonka",
	}, "kumahq/kuma-website", func(t task.Task) {
		got <- t
	})

	select {
	case reviewTask := <-got:
		if reviewTask.ProjectID != "kumahq/kuma-website" {
			t.Fatalf("ProjectID = %q, want kumahq/kuma-website", reviewTask.ProjectID)
		}
		if reviewTask.PRNumber != 2708 {
			t.Fatalf("PRNumber = %d, want 2708", reviewTask.PRNumber)
		}
		if reviewTask.Status != task.StatusTodo {
			t.Fatalf("Status = %q, want %q", reviewTask.Status, task.StatusTodo)
		}
	case <-time.After(time.Second):
		t.Fatal("triage was not called")
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("created files = %d, want 1", len(files))
	}
}

// TestMonitoredPRs covers the regression where Renovate-bot PRs linked to a
// task by pr_number were silently skipped by the pr-monitor because
// FetchReviews uses author:@me. monitoredPRs folds renovatePRsFn output into
// the same slice that drives MatchTaskPRs and DetectClosedTaskPRs.
func TestMonitoredPRs(t *testing.T) {
	mine := github.PullRequest{Number: 1, Author: "me"}
	bot := github.PullRequest{Number: 2, Author: "app/renovate"}
	summary := github.ReviewSummary{CreatedByMe: []github.PullRequest{mine}}

	tests := []struct {
		name      string
		fn        func() []github.PullRequest
		wantNums  []int
		wantAlloc bool // true when fn supplies extra PRs (forces a copy)
	}{
		{
			name:     "nil fn returns CreatedByMe directly",
			fn:       nil,
			wantNums: []int{1},
		},
		{
			name:     "empty fn result also returns CreatedByMe directly",
			fn:       func() []github.PullRequest { return nil },
			wantNums: []int{1},
		},
		{
			name:      "fn returns renovate PRs — concatenated after CreatedByMe",
			fn:        func() []github.PullRequest { return []github.PullRequest{bot} },
			wantNums:  []int{1, 2},
			wantAlloc: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReviewHandler{renovatePRsFn: tt.fn}
			got := r.monitoredPRs(summary)
			if len(got) != len(tt.wantNums) {
				t.Fatalf("len = %d, want %d (got %+v)", len(got), len(tt.wantNums), got)
			}
			for i, n := range tt.wantNums {
				if got[i].Number != n {
					t.Errorf("got[%d].Number = %d, want %d", i, got[i].Number, n)
				}
			}
			// When fn adds entries we must return a fresh slice — appending
			// onto summary.CreatedByMe directly would mutate the caller's
			// data on the next poll.
			if tt.wantAlloc && len(got) > 0 && len(summary.CreatedByMe) > 0 &&
				&got[0] == &summary.CreatedByMe[0] {
				t.Error("monitoredPRs aliased summary.CreatedByMe; expected a copy")
			}
		})
	}
}
