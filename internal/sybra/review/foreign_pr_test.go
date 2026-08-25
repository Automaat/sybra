package review

import (
	"context"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func tagTaskAsReview(t *testing.T, h *autoResolveHarness, taskID string) {
	t.Helper()
	tags := []string{task.TagReview}
	if _, err := h.tasks.Update(taskID, task.Update{Tags: &tags}); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchFixIssues_RefusesReviewTaskOnAnotherAuthorsPR(t *testing.T) {
	// Given a review task whose PR another person opened
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	tagTaskAsReview(t, h, tk.ID)
	pr.Author = "someone-else"
	h.r.viewerLoginFn = func() string { return "sybra-bot" }
	h.r.tryCleanMergeFn = stubMerge(project.CleanMergeConflict, nil)
	h.r.pushSyncFn = stubPush(nil)

	// When a conflict on that PR reaches the fix dispatcher
	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})

	// Then nothing is dispatched against it
	if ok {
		t.Fatal("dispatchFixIssues = true, want false for a PR Sybra does not own")
	}
	got, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatalf("workflow = %+v, want none for a PR Sybra does not own", got.Workflow)
	}
}

func TestDispatchFixIssues_AllowsReviewTaskOnOwnPR(t *testing.T) {
	// Given a review-tagged task whose PR Sybra itself opened
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	tagTaskAsReview(t, h, tk.ID)
	pr.Author = "sybra-bot"
	h.r.viewerLoginFn = func() string { return "sybra-bot" }
	h.r.tryCleanMergeFn = stubMerge(project.CleanMergeConflict, nil)
	h.r.pushSyncFn = stubPush(nil)

	// When a conflict on that PR reaches the fix dispatcher
	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})

	// Then the fix still runs
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true for Sybra's own PR")
	}
}

func TestDispatchFixIssues_AllowsBotPROnNonReviewTask(t *testing.T) {
	// Given a non-review task on a PR a bot opened, such as a dependency update
	h := newAutoResolveHarness(t, true)
	tk, pr := h.newConflictTask(t)
	pr.Author = "renovate[bot]"
	h.r.viewerLoginFn = func() string { return "sybra-bot" }
	h.r.tryCleanMergeFn = stubMerge(project.CleanMergeConflict, nil)
	h.r.pushSyncFn = stubPush(nil)

	// When a conflict on that PR reaches the fix dispatcher
	ok := h.r.dispatchFixIssues(context.Background(), tk.ID, []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: tk.ID, PR: pr},
	})

	// Then the fix still runs
	if !ok {
		t.Fatal("dispatchFixIssues = false, want true for a bot PR on a task Sybra drives")
	}
}
