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
	if err := CreateWorktree(bare, wtPath, "sybra/test-task", branch); err != nil {
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
			raw:     "worktree /tmp/wt\nHEAD abc1234567890\nbranch refs/heads/sybra/a1b2c3d4\n",
			wantLen: 1, wantTaskID: "a1b2c3d4", wantBranch: "sybra/a1b2c3d4",
		},
		{
			name:    "new format slug-id",
			raw:     "worktree /tmp/wt\nHEAD abc1234567890\nbranch refs/heads/sybra/implement-auth-a1b2c3d4\n",
			wantLen: 1, wantTaskID: "a1b2c3d4", wantBranch: "sybra/implement-auth-a1b2c3d4",
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
	if err := CreateWorktree(bare, wtPath, "sybra/test", branch); err != nil {
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
	gitWt("checkout", "sybra/test")

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
	if err := CreateWorktree(bare, wtPath, "sybra/test", branch); err != nil {
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

func TestSanitizeWorktree_AutoCommitsUncommitted(t *testing.T) {
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
	if err := CreateWorktree(bare, wtPath, "sybra/test", branch); err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Configure git identity so commit works.
	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// Simulate agent leaving uncommitted work.
	if err := os.WriteFile(filepath.Join(wtPath, "new_file.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SanitizeWorktree(wtPath); err != nil {
		t.Fatalf("SanitizeWorktree: %v", err)
	}

	// Uncommitted file should now be in a commit, not lost.
	out, err := exec.Command("git", "-C", wtPath, "log", "--oneline", "-1").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(out), "wip:") {
		t.Errorf("expected wip commit, got: %s", out)
	}

	// Working tree should be clean after sanitize.
	statusOut, _ := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Errorf("expected clean working tree, got: %s", statusOut)
	}
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

func initWorktree(t *testing.T) (bare, wtPath string) {
	t.Helper()
	src := initRepoWithCommit(t)
	bare = filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	wtPath = filepath.Join(t.TempDir(), "wt")
	if err := CreateWorktree(bare, wtPath, "synapse/test", branch); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return bare, wtPath
}

func TestMergeChecks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		repo          *ChecksConfig
		app           *ChecksConfig
		wantPreCommit []string
		wantPrePush   []string
		wantNil       bool
	}{
		{
			name:    "both nil",
			wantNil: true,
		},
		{
			name:          "repo only",
			repo:          &ChecksConfig{PreCommit: []string{"echo repo"}},
			wantPreCommit: []string{"echo repo"},
		},
		{
			name:          "app only",
			app:           &ChecksConfig{PreCommit: []string{"echo app"}},
			wantPreCommit: []string{"echo app"},
		},
		{
			name:          "repo wins pre_commit",
			repo:          &ChecksConfig{PreCommit: []string{"echo repo"}},
			app:           &ChecksConfig{PreCommit: []string{"echo app"}},
			wantPreCommit: []string{"echo repo"},
		},
		{
			name:        "repo wins pre_push",
			repo:        &ChecksConfig{PrePush: []string{"echo repo-push"}},
			app:         &ChecksConfig{PrePush: []string{"echo app-push"}},
			wantPrePush: []string{"echo repo-push"},
		},
		{
			name:          "composable: repo pre_commit, app pre_push",
			repo:          &ChecksConfig{PreCommit: []string{"echo repo-commit"}},
			app:           &ChecksConfig{PrePush: []string{"echo app-push"}},
			wantPreCommit: []string{"echo repo-commit"},
			wantPrePush:   []string{"echo app-push"},
		},
		{
			name:          "empty repo slice falls back to app",
			repo:          &ChecksConfig{PreCommit: []string{}},
			app:           &ChecksConfig{PreCommit: []string{"echo app"}},
			wantPreCommit: []string{"echo app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := MergeChecks(tt.repo, tt.app)
			if tt.wantNil {
				if got != nil {
					t.Errorf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want non-nil")
			}
			if !slicesEqual(got.PreCommit, tt.wantPreCommit) {
				t.Errorf("PreCommit = %v, want %v", got.PreCommit, tt.wantPreCommit)
			}
			if !slicesEqual(got.PrePush, tt.wantPrePush) {
				t.Errorf("PrePush = %v, want %v", got.PrePush, tt.wantPrePush)
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoadRepoConfig_Missing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil || cfg.Checks != nil {
		t.Errorf("expected empty RepoConfig, got %+v", cfg)
	}
}

func TestLoadRepoConfig_Valid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "checks:\n  pre_commit:\n    - echo hello\n  pre_push:\n    - echo world\n"
	if err := os.WriteFile(filepath.Join(dir, ".synapse.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Checks == nil {
		t.Fatal("expected checks, got nil")
	}
	if len(cfg.Checks.PreCommit) != 1 || cfg.Checks.PreCommit[0] != "echo hello" {
		t.Errorf("PreCommit = %v", cfg.Checks.PreCommit)
	}
	if len(cfg.Checks.PrePush) != 1 || cfg.Checks.PrePush[0] != "echo world" {
		t.Errorf("PrePush = %v", cfg.Checks.PrePush)
	}
}

func TestLoadRepoConfig_Invalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".synapse.yaml"), []byte(":\n  bad: [yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRepoConfig(dir)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestInstallHooks_RepoConfigPriority(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}
	_, wtPath := initWorktree(t)

	// Write .synapse.yaml with a failing pre-commit to prove repo config is used.
	repoYAML := "checks:\n  pre_commit:\n    - exit 1\n"
	if err := os.WriteFile(filepath.Join(wtPath, ".synapse.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// App config has a passing pre-commit — repo should win.
	appChecks := &ChecksConfig{PreCommit: []string{"exit 0"}}
	repoCfg, err := LoadRepoConfig(wtPath)
	if err != nil {
		t.Fatalf("LoadRepoConfig: %v", err)
	}
	merged := MergeChecks(repoCfg.Checks, appChecks)
	if err := InstallHooks(wtPath, merged); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "change.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	addCmd := exec.Command("git", "add", ".")
	addCmd.Dir = wtPath
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitCmd := exec.Command("git", "commit", "--no-gpg-sign", "-m", "test")
	commitCmd.Dir = wtPath
	if err := commitCmd.Run(); err == nil {
		t.Fatal("commit should have been blocked by repo pre-commit hook (exit 1)")
	}
}

func TestInstallHooks_NilChecks(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}
	_, wtPath := initWorktree(t)
	if err := InstallHooks(wtPath, nil); err != nil {
		t.Fatalf("InstallHooks(nil): %v", err)
	}
}

func TestInstallHooks_EmptySlices(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}
	_, wtPath := initWorktree(t)
	if err := InstallHooks(wtPath, &ChecksConfig{}); err != nil {
		t.Fatalf("InstallHooks(empty): %v", err)
	}

	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = wtPath
	out, _ := cmd.Output()
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wtPath, gitDir)
	}
	for _, name := range []string{"pre-commit", "pre-push"} {
		if _, err := os.Stat(filepath.Join(gitDir, "hooks", name)); err == nil {
			t.Errorf("hook %s should not exist for empty config", name)
		}
	}
}

func TestInstallHooks_PreCommitBlocksOnFailure(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}
	_, wtPath := initWorktree(t)

	checks := &ChecksConfig{
		PreCommit: []string{"exit 1"},
	}
	if err := InstallHooks(wtPath, checks); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	// Verify hook file exists and is executable.
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = wtPath
	out, _ := cmd.Output()
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wtPath, gitDir)
	}
	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("pre-commit hook missing: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("pre-commit hook not executable")
	}

	// Commit should be blocked by the failing hook.
	if err := os.WriteFile(filepath.Join(wtPath, "change.txt"), []byte("change"), 0o644); err != nil {
		t.Fatal(err)
	}
	addCmd := exec.Command("git", "add", ".")
	addCmd.Dir = wtPath
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitCmd := exec.Command("git", "commit", "--no-gpg-sign", "-m", "test")
	commitCmd.Dir = wtPath
	if err := commitCmd.Run(); err == nil {
		t.Fatal("expected commit to fail due to pre-commit hook")
	}
}

func TestInstallHooks_PreCommitPassesOnSuccess(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}
	_, wtPath := initWorktree(t)

	checks := &ChecksConfig{
		PreCommit: []string{"exit 0"},
	}
	if err := InstallHooks(wtPath, checks); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "change.txt"), []byte("change"), 0o644); err != nil {
		t.Fatal(err)
	}
	addCmd := exec.Command("git", "add", ".")
	addCmd.Dir = wtPath
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitCmd := exec.Command("git", "commit", "--no-gpg-sign", "-m", "test")
	commitCmd.Dir = wtPath
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("commit should succeed with passing hook: %v: %s", err, out)
	}
}

func TestInstallHooks_PrePushInstalled(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}
	_, wtPath := initWorktree(t)

	checks := &ChecksConfig{
		PrePush: []string{"echo pre-push ok"},
	}
	if err := InstallHooks(wtPath, checks); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = wtPath
	out, _ := cmd.Output()
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wtPath, gitDir)
	}
	hookPath := filepath.Join(gitDir, "hooks", "pre-push")
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("pre-push hook missing: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("pre-push hook not executable")
	}
	content, _ := os.ReadFile(hookPath)
	if !strings.Contains(string(content), "echo pre-push ok") {
		t.Errorf("hook content missing command: %s", content)
	}
}

func TestInstallHooks_Overwrites(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}
	_, wtPath := initWorktree(t)

	// Install first version.
	if err := InstallHooks(wtPath, &ChecksConfig{PreCommit: []string{"echo v1"}}); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Overwrite with second version.
	if err := InstallHooks(wtPath, &ChecksConfig{PreCommit: []string{"echo v2"}}); err != nil {
		t.Fatalf("second install: %v", err)
	}

	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = wtPath
	out, _ := cmd.Output()
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wtPath, gitDir)
	}
	content, _ := os.ReadFile(filepath.Join(gitDir, "hooks", "pre-commit"))
	if strings.Contains(string(content), "v1") {
		t.Error("hook should have been overwritten with v2")
	}
	if !strings.Contains(string(content), "v2") {
		t.Errorf("hook should contain v2: %s", content)
	}
}
