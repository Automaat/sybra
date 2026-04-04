package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseGitHubURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"https", "https://github.com/owner/repo", "owner", "repo", false},
		{"https with .git", "https://github.com/owner/repo.git", "owner", "repo", false},
		{"https trailing slash", "https://github.com/owner/repo/", "owner", "repo", false},
		{"ssh", "git@github.com:owner/repo.git", "owner", "repo", false},
		{"ssh no .git", "git@github.com:owner/repo", "owner", "repo", false},
		{"with spaces", "  https://github.com/owner/repo  ", "owner", "repo", false},
		{"not github", "https://gitlab.com/owner/repo", "", "", true},
		{"missing repo", "https://github.com/owner", "", "", true},
		{"empty path", "https://github.com/", "", "", true},
		{"empty string", "", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := ParseGitHubURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path      string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"owner/repo", "owner", "repo", false},
		{"owner/repo/extra", "owner", "repo", false},
		{"owner/", "", "", true},
		{"/repo", "", "", true},
		{"noslash", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := splitOwnerRepo(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}

func hasGit() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func initBareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "test.git")
	cmd := exec.Command("git", "init", "--bare", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	return dir
}

func initRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", dir},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", dir, "add", "."},
		{"git", "-C", dir, "commit", "-m", "init"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestCloneBare(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	src := initRepoWithCommit(t)
	dest := filepath.Join(t.TempDir(), "clone.git")

	if err := CloneBare(src, dest); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "HEAD")); err != nil {
		t.Error("bare clone missing HEAD file")
	}
}

func TestCloneBareInvalidURL(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	dest := filepath.Join(t.TempDir(), "clone.git")
	if err := CloneBare("/nonexistent/repo", dest); err == nil {
		t.Fatal("expected error for invalid source")
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	bare := initBareRepo(t)
	branch, err := DefaultBranch(bare)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch == "" {
		t.Error("branch is empty")
	}
}

func TestFetchOriginNoRemote(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	bare := initBareRepo(t)
	err := FetchOrigin(bare)
	if err == nil {
		t.Fatal("expected error fetching from repo with no origin")
	}
}

func TestCreateAndRemoveWorktree(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	branch, err := DefaultBranch(bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "worktree")
	if err := CreateWorktree(bare, wtPath, "synapse/test-task", branch); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Error("worktree missing README.md")
	}

	if err := RemoveWorktree(bare, wtPath); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Error("worktree dir should be removed")
	}
}

func TestParseWorktreePorcelain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		raw        string
		wantLen    int
		wantTaskID string
		wantBranch string
	}{
		{
			name:    "old format bare id",
			raw:     "worktree /tmp/wt\nHEAD abc1234567890\nbranch refs/heads/synapse/a1b2c3d4\n",
			wantLen: 1, wantTaskID: "a1b2c3d4", wantBranch: "synapse/a1b2c3d4",
		},
		{
			name:    "new format slug-id",
			raw:     "worktree /tmp/wt\nHEAD abc1234567890\nbranch refs/heads/synapse/implement-auth-a1b2c3d4\n",
			wantLen: 1, wantTaskID: "a1b2c3d4", wantBranch: "synapse/implement-auth-a1b2c3d4",
		},
		{
			name:    "non-synapse branch",
			raw:     "worktree /tmp/wt\nHEAD abc1234567890\nbranch refs/heads/feature/foo\n",
			wantLen: 1, wantTaskID: "", wantBranch: "feature/foo",
		},
		{
			name:    "bare entry skipped",
			raw:     "worktree /tmp/bare.git\nbare\n",
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseWorktreePorcelain(tt.raw)
			if len(got) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen == 0 {
				return
			}
			if got[0].TaskID != tt.wantTaskID {
				t.Errorf("TaskID = %q, want %q", got[0].TaskID, tt.wantTaskID)
			}
			if got[0].Branch != tt.wantBranch {
				t.Errorf("Branch = %q, want %q", got[0].Branch, tt.wantBranch)
			}
		})
	}
}

func TestSanitizeWorktree_AbortsRebase(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	branch, _ := DefaultBranch(bare)
	if err := CreateWorktree(bare, wtPath, "synapse/test", branch); err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Create a conflicting commit on main.
	gitWt := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	gitWt("config", "user.email", "test@test.com")
	gitWt("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("branch change"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitWt("add", ".")
	gitWt("commit", "-m", "branch")

	// Make a conflicting commit on a new branch from original base.
	gitWt("checkout", "-b", "conflict-base", "HEAD~1")
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("conflicting"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitWt("add", ".")
	gitWt("commit", "-m", "conflict")
	gitWt("checkout", "synapse/test")

	// Start a rebase that will conflict.
	cmd := exec.Command("git", "rebase", "conflict-base")
	cmd.Dir = wtPath
	_ = cmd.Run() // expected to fail with conflict

	// Verify rebase is in progress.
	statusOut, _ := exec.Command("git", "-C", wtPath, "status").Output()
	if !contains(string(statusOut), "rebase") {
		t.Skip("could not create rebase conflict state")
	}

	if err := SanitizeWorktree(wtPath); err != nil {
		t.Fatalf("SanitizeWorktree: %v", err)
	}

	// Rebase should be aborted.
	statusOut, _ = exec.Command("git", "-C", wtPath, "status").Output()
	if contains(string(statusOut), "rebase") {
		t.Error("rebase still in progress after sanitize")
	}
}

func TestSanitizeWorktree_DeletesShadowBranches(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	branch, _ := DefaultBranch(bare)
	if err := CreateWorktree(bare, wtPath, "synapse/test", branch); err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Create a local branch that shadows origin/main.
	cmd := exec.Command("git", "branch", "origin/main", "HEAD")
	cmd.Dir = wtPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create shadow branch: %v: %s", err, out)
	}

	if err := SanitizeWorktree(wtPath); err != nil {
		t.Fatalf("SanitizeWorktree: %v", err)
	}

	// Shadow branch should be deleted.
	out, _ := exec.Command("git", "-C", wtPath, "branch", "--list", "origin/main").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("shadow branch origin/main still exists: %s", out)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestCreateWorktreeInvalidBase(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	bare := initBareRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	err := CreateWorktree(bare, wtPath, "test-branch", "nonexistent-base")
	if err == nil {
		t.Fatal("expected error for invalid base branch")
	}
}
