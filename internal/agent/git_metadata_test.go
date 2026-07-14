package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestGitMetadataRoots_LinkedWorktreeIncludesGitdirAndCommonDir(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "worktrees", "task-1")
	gitDir := filepath.Join(root, "clones", "repo.git", "worktrees", "task-1")
	commonDir := filepath.Join(root, "clones", "repo.git")

	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := gitMetadataRoots(worktree)
	if err != nil {
		t.Fatalf("gitMetadataRoots: %v", err)
	}
	want := []string{gitDir, commonDir}
	for i, root := range want {
		canon, err := canonicalizeRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = canon
	}
	if !slices.Equal(got, want) {
		t.Fatalf("gitMetadataRoots = %v, want %v", got, want)
	}
}

func TestGitMetadataRoots_NonGitWorktreeReturnsEmpty(t *testing.T) {
	got, err := gitMetadataRoots(t.TempDir())
	if err != nil {
		t.Fatalf("gitMetadataRoots: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("gitMetadataRoots = %v, want empty", got)
	}
}
