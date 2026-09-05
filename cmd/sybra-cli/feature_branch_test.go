package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
)

// TestAssertFeatureBranch is the handoff-side half of the default-branch guard
// (#3129). It compares the worktree's branch against project.DefaultBranch, so
// while that truncated refs/heads/release/2.0 to "2.0" the comparison never
// matched and a handoff from a checkout on the default branch was accepted.
func TestAssertFeatureBranch(t *testing.T) {
	for _, tt := range []struct {
		name          string
		defaultBranch string
		checkout      string
		wantErr       string
	}{
		{name: "slashed default refused", defaultBranch: "release/2.0", checkout: "release/2.0", wantErr: `on the default branch "release/2.0"`},
		{name: "plain default refused", defaultBranch: "main", checkout: "main", wantErr: `on the default branch "main"`},
		{name: "feature branch accepted", defaultBranch: "release/2.0", checkout: "feat/work"},
		{name: "branch sharing the default's last segment accepted", defaultBranch: "release/2.0", checkout: "hotfix/2.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			clone := filepath.Join(tmp, "clone.git")
			work := filepath.Join(tmp, "work")

			runGit(t, "", "init", "-b", tt.defaultBranch, work)
			runGit(t, work, "config", "user.email", "test@test.com")
			runGit(t, work, "config", "user.name", "Test")
			runGit(t, work, "config", "commit.gpgsign", "false")
			if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, work, "add", ".")
			runGit(t, work, "commit", "-m", "init")
			runGit(t, "", "clone", "--bare", work, clone)
			if tt.checkout != tt.defaultBranch {
				runGit(t, work, "checkout", "-b", tt.checkout)
			}

			proj := project.Project{ID: "owner/repo", ClonePath: clone}
			err := assertFeatureBranch(work, proj)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("assertFeatureBranch on %q: %v", tt.checkout, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("handoff accepted a worktree on the default branch %q", tt.defaultBranch)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestAssertFeatureBranch_NoClonePathFailsClosed pins the fail-closed contract:
// a project record with no clone path must be a diagnosable refusal. Left to
// git, an empty path runs in the CLI's own cwd, so the guard would compare the
// worktree's branch against whatever branch that checkout happens to be on.
func TestAssertFeatureBranch_NoClonePathFailsClosed(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	runGit(t, "", "init", "-b", "main", work)
	runGit(t, work, "checkout", "-b", "feat/work")

	err := assertFeatureBranch(work, project.Project{ID: "owner/repo"})
	if err == nil {
		t.Fatal("handoff accepted a project with no clone path")
	}
	if !strings.Contains(err.Error(), "clone path") {
		t.Errorf("error = %q, want it to name the missing clone path", err)
	}
}
