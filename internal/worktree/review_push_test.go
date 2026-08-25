package worktree

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func remoteHasBranch(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--verify", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func prepareTaskOnBranch(t *testing.T, h preparedHarness, title, branch string, u task.Update) task.Task {
	t.Helper()
	tk, err := h.tasks.Store().Create(title, "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	u.ProjectID = task.Ptr(h.proj.ID)
	u.Branch = task.Ptr(branch)
	if _, err := h.tasks.Update(tk.ID, u); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestPrepareForTask_PushesOwnBranch(t *testing.T) {
	// Given a task Sybra owns, on a branch of its own
	h := prepareHarness(t, nil, 30*time.Second)
	tk := prepareTaskOnBranch(t, h, "own work", "fix/ours", task.Update{})

	// When the worktree is prepared
	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}

	// Then the branch reaches the remote
	if !remoteHasBranch(t, h.src, "fix/ours") {
		t.Fatal("own task branch was not pushed to the remote")
	}
}

func TestPrepareForTask_SkipsPushForPRReviewTask(t *testing.T) {
	// Given a review task carrying another author's PR branch
	h := prepareHarness(t, nil, 30*time.Second)
	tags := []string{task.TagReview}
	tk := prepareTaskOnBranch(t, h, "Review: their work", "feat/theirs", task.Update{
		Tags:     &tags,
		PRNumber: task.Ptr(382),
	})

	// When the worktree is prepared
	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}

	// Then nothing is pushed to that branch
	if remoteHasBranch(t, h.src, "feat/theirs") {
		t.Fatal("review task pushed to a branch it does not own")
	}
}
