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

	t.Run("staged marker-free merge path is recoverable", func(t *testing.T) {
		t.Parallel()
		wtPath := newConflictedWorktree(t)
		if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("feature\nmain\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitInDir(t, wtPath, "add", "README.md")

		got, err := ResolvedUnmergedPaths(context.Background(), wtPath)
		if err != nil {
			t.Fatalf("ResolvedUnmergedPaths: %v", err)
		}
		if !slices.Equal(got, []string{"README.md"}) {
			t.Fatalf("paths = %v, want [README.md]", got)
		}
	})

	t.Run("marker-free unstaged path is recoverable", func(t *testing.T) {
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
			t.Fatalf("paths = %v, want [README.md] even though it was never `git add`ed", got)
		}
	})

	t.Run("deleted unstaged unmerged path is not recoverable", func(t *testing.T) {
		t.Parallel()
		wtPath := newConflictedWorktree(t)
		if err := os.Remove(filepath.Join(wtPath, "README.md")); err != nil {
			t.Fatal(err)
		}

		got, err := ResolvedUnmergedPaths(context.Background(), wtPath)
		if err != nil {
			t.Fatalf("ResolvedUnmergedPaths: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("paths = %v, want none when the resolution deleted the file", got)
		}
	})

	t.Run("marker-free binary conflict is not recoverable", func(t *testing.T) {
		t.Parallel()
		wtPath := newBinaryConflictedWorktree(t)

		got, err := ResolvedUnmergedPaths(context.Background(), wtPath)
		if err != nil {
			t.Fatalf("ResolvedUnmergedPaths: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("paths = %v, want none while binary conflict remains unmerged", got)
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

func newBinaryConflictedWorktree(t *testing.T) string {
	t.Helper()
	bare, featureWT := initWorktree(t)
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	writeBytesAndCommit(t, featureWT, "a.bin", []byte{0, 1, 2, 'f'}, "feature binary edit")

	baseWT := initSecondWorktree(t, bare, branch, branch)
	writeBytesAndCommit(t, baseWT, "a.bin", []byte{0, 1, 2, 'm'}, "base binary edit")

	cmd := exec.Command("git", "merge", "refs/heads/"+branch)
	cmd.Dir = featureWT
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git merge unexpectedly succeeded: %s", out)
	}
	return featureWT
}

func writeBytesAndCommit(t *testing.T, dir, file string, content []byte, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), content, 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	runGitInDir(t, dir, "add", file)
	runGitInDir(t, dir, "commit", "-m", message)
}

func runGitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
