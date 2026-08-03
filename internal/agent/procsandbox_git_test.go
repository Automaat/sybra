package agent

import (
	"context"
	"os/exec"
	"slices"
	"testing"
)

func TestDedupeRoots_DropsEmptyAndDuplicate(t *testing.T) {
	got := dedupeRoots("/a", "/a", "", "/b", "  ", "/a")
	if !slices.Equal(got, []string{"/a", "/b"}) {
		t.Errorf("dedupeRoots = %v, want [/a /b]", got)
	}
}

// A review worktree checked out at a pull-request head has no branch. Treating
// that as an error failed the entire run closed under sandbox enforce with a
// bare "git symbolic-ref -q HEAD: exit status 1".
func TestResolveGitSharedWritablePaths_DetachedHeadIsNotAnError(t *testing.T) {
	wt := t.TempDir()
	runGitOrFail(t, wt, "init", "-q", ".")
	runGitOrFail(t, wt, "config", "user.email", "t@example.com")
	runGitOrFail(t, wt, "config", "user.name", "T")
	runGitOrFail(t, wt, "config", "commit.gpgsign", "false")
	runGitOrFail(t, wt, "commit", "-q", "--allow-empty", "-m", "seed")

	attached, err := resolveGitSharedWritablePaths(context.Background(), wt)
	if err != nil {
		t.Fatalf("attached HEAD: %v", err)
	}
	if attached.branchRef == "" {
		t.Fatal("attached HEAD resolved an empty branch ref")
	}

	runGitOrFail(t, wt, "checkout", "-q", "--detach")

	detached, err := resolveGitSharedWritablePaths(context.Background(), wt)
	if err != nil {
		t.Fatalf("detached HEAD must not fail sandbox setup: %v", err)
	}
	if detached.branchRef != "" {
		t.Fatalf("branchRef = %q, want empty on a detached HEAD", detached.branchRef)
	}
	// The object dir is what the run actually needs; only the branch-scoped
	// grants drop away.
	if detached.objectDir == "" {
		t.Fatal("detached HEAD lost the git object dir grant")
	}
}

// Exit 128 (not a repository) must still fail rather than be read as detached.
func TestGitSymbolicRef_RealFailureStillErrors(t *testing.T) {
	if _, err := gitSymbolicRef(context.Background(), t.TempDir()); err == nil {
		t.Fatal("gitSymbolicRef succeeded outside a git repository")
	}
}

func runGitOrFail(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
