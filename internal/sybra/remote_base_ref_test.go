package sybra

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func newDetachedCheckout(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	run("config", "commit.gpgsign", "false")
	run("commit", "--allow-empty", "-m", "base")
	sha = run("rev-parse", "HEAD")
	run("checkout", "--detach", sha)
	return dir, sha
}

func TestRemoteBaseRef_DetachedReviewCheckoutNamesThePRHead(t *testing.T) {
	// Given a review task on a detached checkout of a pull request head
	dir, _ := newDetachedCheckout(t)
	review := task.Task{ID: "t1", PRNumber: 382}

	// When its remote workspace base ref is resolved
	ref, err := remoteBaseRef(context.Background(), review, dir)

	// Then the pull request head names it instead of failing on symbolic-ref
	if err != nil {
		t.Fatalf("remoteBaseRef: %v", err)
	}
	if ref != "refs/pull/382/head" {
		t.Fatalf("ref = %q, want refs/pull/382/head", ref)
	}
}

func TestRemoteBaseRef_PrefersTheTaskBranch(t *testing.T) {
	// Given a task carrying its own branch
	dir, _ := newDetachedCheckout(t)
	owned := task.Task{ID: "t1", Branch: "feat/mine", PRNumber: 7}

	// When the base ref is resolved
	ref, err := remoteBaseRef(context.Background(), owned, dir)

	// Then the branch wins over both the checkout and the pull request
	if err != nil {
		t.Fatalf("remoteBaseRef: %v", err)
	}
	if ref != "refs/heads/feat/mine" {
		t.Fatalf("ref = %q, want refs/heads/feat/mine", ref)
	}
}

func TestRemoteBaseRef_ReadsAnAttachedCheckout(t *testing.T) {
	// Given a branchless task on an ordinary attached checkout
	dir, sha := newDetachedCheckout(t)
	cmd := exec.Command("git", "checkout", "-B", "feat/attached", sha)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v\n%s", err, out)
	}

	// When the base ref is resolved
	ref, err := remoteBaseRef(context.Background(), task.Task{ID: "t1"}, dir)

	// Then the checked-out branch names it
	if err != nil {
		t.Fatalf("remoteBaseRef: %v", err)
	}
	if ref != "refs/heads/feat/attached" {
		t.Fatalf("ref = %q, want refs/heads/feat/attached", ref)
	}
}

func TestRemoteBaseRef_DetachedWithoutAPullRequestSaysSo(t *testing.T) {
	// Given a detached checkout with neither a branch nor a pull request
	dir, _ := newDetachedCheckout(t)

	// When the base ref is resolved
	_, err := remoteBaseRef(context.Background(), task.Task{ID: "t1"}, dir)

	// Then the refusal names the real condition, not a git plumbing failure
	if err == nil {
		t.Fatal("remoteBaseRef accepted a checkout it cannot name")
	}
	if !strings.Contains(err.Error(), "no branch or pull request") {
		t.Fatalf("error = %v, want it to name the missing branch and pull request", err)
	}
	if strings.Contains(err.Error(), "symbolic-ref") {
		t.Fatalf("error = %v, want no git plumbing detail", err)
	}
}

func TestRemoteBaseRef_ResultIsAValidFullRef(t *testing.T) {
	// Given every shape the resolver can return
	dir, _ := newDetachedCheckout(t)
	cases := []task.Task{
		{ID: "t1", PRNumber: 382},
		{ID: "t2", Branch: "feat/mine"},
	}

	for _, tk := range cases {
		// When the base ref is resolved
		ref, err := remoteBaseRef(context.Background(), tk, dir)
		if err != nil {
			t.Fatalf("remoteBaseRef(%+v): %v", tk, err)
		}

		// Then it is a full ref the execution contract accepts
		if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") {
			t.Fatalf("ref = %q, want a valid full git ref", ref)
		}
		if filepath.Clean(ref) != ref {
			t.Fatalf("ref = %q, want a clean ref path", ref)
		}
	}
}
