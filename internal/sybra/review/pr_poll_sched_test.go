package review

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

func TestExpBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		streak   int
		maxTicks int
		want     int
	}{
		{name: "non-positive streak", streak: 0, maxTicks: 8, want: 0},
		{name: "first stable poll", streak: 1, maxTicks: 8, want: 1},
		{name: "second stable poll", streak: 2, maxTicks: 8, want: 2},
		{name: "third stable poll", streak: 3, maxTicks: 8, want: 4},
		{name: "clamps to max", streak: 5, maxTicks: 8, want: 8},
		{name: "disabled when max non-positive", streak: 3, maxTicks: 0, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := expBackoff(tt.streak, tt.maxTicks); got != tt.want {
				t.Fatalf("expBackoff(%d, %d) = %d, want %d", tt.streak, tt.maxTicks, got, tt.want)
			}
		})
	}
}

func TestSelectKnownPRPoll(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	newTask := func(id string, pr int, updatedAt time.Time) task.Task {
		return task.Task{
			ID:        id,
			ProjectID: "owner/repo",
			PRNumber:  pr,
			Status:    task.StatusInReview,
			UpdatedAt: updatedAt,
		}
	}

	t.Run("active bypasses cap and recent tasks win", func(t *testing.T) {
		t.Parallel()

		agents := newTestAgentManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
		claim, ok := agents.TryClaimDispatch("active")
		if !ok {
			t.Fatal("TryClaimDispatch(active) = false, want true")
		}
		defer claim.Release()

		r := &Handler{
			agents: agents,
			cfg: &config.Config{
				GitHub: config.GitHubConfig{ReviewsMaxPRsPerTick: 2},
			},
		}

		sel := r.selectKnownPRPoll([]task.Task{
			newTask("older", 3, base.Add(-2*time.Hour)),
			newTask("active", 1, base.Add(-4*time.Hour)),
			newTask("newest", 2, base),
		})

		if got := taskIDs(sel.tasks); len(got) != 2 || got[0] != "active" || got[1] != "newest" {
			t.Fatalf("selected ids = %v, want [active newest]", got)
		}
		if sel.selectedPRs != 2 || sel.deferredPRs != 0 || sel.cappedPRs != 1 {
			t.Fatalf("selection stats = %+v, want selected=2 deferred=0 capped=1", sel)
		}
	})

	t.Run("priority ordering and cap truncation", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			cfg: &config.Config{
				GitHub: config.GitHubConfig{ReviewsMaxPRsPerTick: 2},
			},
		}

		sel := r.selectKnownPRPoll([]task.Task{
			newTask("mid", 2, base.Add(-time.Minute)),
			newTask("old", 3, base.Add(-2*time.Minute)),
			newTask("new", 1, base),
		})

		if got := taskIDs(sel.tasks); len(got) != 2 || got[0] != "new" || got[1] != "mid" {
			t.Fatalf("selected ids = %v, want [new mid]", got)
		}
		if sel.cappedPRs != 1 {
			t.Fatalf("cappedPRs = %d, want 1", sel.cappedPRs)
		}
	})

	t.Run("ineligible PR-linked tasks do not consume cap", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			cfg: &config.Config{
				GitHub: config.GitHubConfig{ReviewsMaxPRsPerTick: 2},
			},
		}

		done := newTask("done", 1, base)
		done.Status = task.StatusDone
		chat := newTask("chat", 2, base.Add(-time.Second))
		chat.TaskType = task.TaskTypeChat
		reviewTask := newTask("review-task", 3, base.Add(-2*time.Second))
		reviewTask.Tags = []string{"review"}

		sel := r.selectKnownPRPoll([]task.Task{
			done,
			chat,
			reviewTask,
			newTask("eligible-new", 4, base.Add(-time.Minute)),
			newTask("eligible-mid", 5, base.Add(-2*time.Minute)),
			newTask("eligible-old", 6, base.Add(-3*time.Minute)),
		})

		if got := eligibleTaskIDs(sel.tasks); len(got) != 2 || got[0] != "eligible-new" || got[1] != "eligible-mid" {
			t.Fatalf("eligible selected ids = %v, want [eligible-new eligible-mid]", got)
		}
		if sel.selectedPRs != 2 || sel.cappedPRs != 1 {
			t.Fatalf("selection stats = %+v, want selected=2 capped=1", sel)
		}
	})

	t.Run("deferred countdown becomes eligible again", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			prPollState: map[string]prPollEntry{
				"owner/repo#7": {skipTicks: 2},
			},
		}
		tk := newTask("deferred", 7, base)

		for i, wantSkip := range []int{1, 0} {
			sel := r.selectKnownPRPoll([]task.Task{tk})
			if len(sel.tasks) != 0 {
				t.Fatalf("poll %d selected ids = %v, want none while deferred", i+1, taskIDs(sel.tasks))
			}
			if sel.deferredPRs != 1 {
				t.Fatalf("poll %d deferredPRs = %d, want 1", i+1, sel.deferredPRs)
			}
			if got := r.prPollState["owner/repo#7"].skipTicks; got != wantSkip {
				t.Fatalf("poll %d skipTicks = %d, want %d", i+1, got, wantSkip)
			}
		}

		sel := r.selectKnownPRPoll([]task.Task{tk})
		if got := taskIDs(sel.tasks); len(got) != 1 || got[0] != "deferred" {
			t.Fatalf("selected ids = %v, want [deferred] after countdown", got)
		}
	})

	t.Run("prunes unlinked PRs", func(t *testing.T) {
		t.Parallel()

		r := &Handler{
			prPollState: map[string]prPollEntry{
				"owner/repo#1": {},
				"owner/repo#2": {},
			},
		}

		r.pruneKnownPRState(map[string]struct{}{"owner/repo#1": {}})
		if len(r.prPollState) != 1 {
			t.Fatalf("len(prPollState) = %d, want 1", len(r.prPollState))
		}
		if _, ok := r.prPollState["owner/repo#1"]; !ok {
			t.Fatal("owner/repo#1 pruned, want retained")
		}
		if _, ok := r.prPollState["owner/repo#2"]; ok {
			t.Fatal("owner/repo#2 retained, want pruned")
		}
	})

	t.Run("50 PR board simulation", func(t *testing.T) {
		t.Parallel()

		agents := newTestAgentManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
		claim, ok := agents.TryClaimDispatch("active-1")
		if !ok {
			t.Fatal("TryClaimDispatch(active-1) = false, want true")
		}
		defer claim.Release()

		r := &Handler{
			agents: agents,
			cfg: &config.Config{
				GitHub: config.GitHubConfig{
					ReviewsMaxPRsPerTick:         25,
					ReviewsStableBackoffMaxTicks: 8,
				},
			},
		}

		tasks := make([]task.Task, 0, 50)
		for i := range 50 {
			tasks = append(tasks, newTask(
				taskIDForBoard(i),
				i+1,
				base.Add(time.Duration(-i)*time.Minute),
			))
		}
		tasks[0].ID = "active-1"

		sel := r.selectKnownPRPoll(tasks)
		if sel.selectedPRs != 25 {
			t.Fatalf("selectedPRs = %d, want 25", sel.selectedPRs)
		}
		if sel.cappedPRs != 25 {
			t.Fatalf("cappedPRs = %d, want 25", sel.cappedPRs)
		}
		if got := taskIDs(sel.tasks); len(got) != 25 || got[0] != "active-1" {
			t.Fatalf("selected ids len/head = %d/%q, want 25/active-1", len(got), firstID(got))
		}

		stablePR := github.PullRequest{HeadSHA: "sha-1", UpdatedAt: "2026-07-13T12:00:00Z"}
		r.noteKnownPRResult("owner/repo", 42, stablePR)
		wantSkips := []int{1, 2, 4, 8}
		for i, want := range wantSkips {
			r.noteKnownPRResult("owner/repo", 42, stablePR)
			if got := r.prPollState["owner/repo#42"].skipTicks; got != want {
				t.Fatalf("stable round %d skipTicks = %d, want %d", i+1, got, want)
			}
		}

		changedPR := stablePR
		changedPR.HeadSHA = "sha-2"
		r.noteKnownPRResult("owner/repo", 42, changedPR)
		if got := r.prPollState["owner/repo#42"].skipTicks; got != 0 {
			t.Fatalf("skipTicks after head change = %d, want 0", got)
		}
	})
}

func taskIDs(tasks []task.Task) []string {
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
	}
	return ids
}

func eligibleTaskIDs(tasks []task.Task) []string {
	ids := make([]string, 0, len(tasks))
	for i := range tasks {
		if knownPRPollEligible(&tasks[i]) {
			ids = append(ids, tasks[i].ID)
		}
	}
	return ids
}

func taskIDForBoard(i int) string {
	return fmt.Sprintf("task-%02d", i)
}

func firstID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
