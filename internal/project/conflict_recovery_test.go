package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestResolvedUnmergedPaths(t *testing.T) {
	t.Parallel()

	t.Run("marker-free unmerged path is recoverable", func(t *testing.T) {
		t.Parallel()
		wtPath := newConflictedWorktree(t)
		if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("feature\nmain\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := ResolvedUnmergedPaths(context.Background(), wtPath)
		if err != nil {
			t.Fatalf("ResolvedUnmergedPaths: %v", err)
		}
		if !slices.Equal(got, []string{"README.md"}) {
			t.Fatalf("paths = %v, want [README.md]", got)
		}
	})

	t.Run("still-markered conflict is not recoverable", func(t *testing.T) {
		t.Parallel()
		wtPath := newConflictedWorktree(t)

		got, err := ResolvedUnmergedPaths(context.Background(), wtPath)
		if err != nil {
			t.Fatalf("ResolvedUnmergedPaths: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("paths = %v, want none while markers remain", got)
		}
	})

	t.Run("clean tree reports nothing", func(t *testing.T) {
		t.Parallel()
		_, wtPath := initWorktree(t)

		got, err := ResolvedUnmergedPaths(context.Background(), wtPath)
		if err != nil {
			t.Fatalf("ResolvedUnmergedPaths: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("paths = %v, want none on a clean tree", got)
		}
	})
}

func newConflictedWorktree(t *testing.T) string {
	t.Helper()
	bare, featureWT := initWorktree(t)
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	writeAndCommit(t, featureWT, "README.md", "feature\n", "feature edit")

	baseWT := initSecondWorktree(t, bare, branch, branch)
	writeAndCommit(t, baseWT, "README.md", "main\n", "base edit")

	cmd := exec.Command("git", "merge", "refs/heads/"+branch)
	cmd.Dir = featureWT
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git merge unexpectedly succeeded: %s", out)
	}
	return featureWT
}
