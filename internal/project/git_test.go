package project

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		{"https with .git trailing slash", "https://github.com/owner/repo.git/", "owner", "repo", false},
		{"ssh", "git@github.com:owner/repo.git", "owner", "repo", false},
		{"ssh no .git", "git@github.com:owner/repo", "owner", "repo", false},
		{"ssh with .git trailing slash", "git@github.com:owner/repo.git/", "owner", "repo", false},
		{"with spaces", "  https://github.com/owner/repo  ", "owner", "repo", false},
		{"not github", "https://gitlab.com/owner/repo", "", "", true},
		{"missing repo", "https://github.com/owner", "", "", true},
		{"empty path", "https://github.com/", "", "", true},
		{"empty string", "", "", "", true},
		// path traversal / malicious inputs
		{"dotdot owner", "https://github.com/../repo", "", "", true},
		{"dotdot repo", "https://github.com/owner/..", "", "", true},
		{"extra path segments", "https://github.com/owner/repo/extra", "", "", true},
		{"path traversal extra segments", "https://github.com/owner/repo/../../etc/passwd", "", "", true},
		{"ssh dotdot owner", "git@github.com:../malicious.git", "", "", true},
		{"ssh extra segments", "git@github.com:owner/repo/extra.git", "", "", true},
		{"dot in owner", "https://github.com/own.er/repo", "", "", true},
		{"special char in repo", "https://github.com/owner/re@po", "", "", true},
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
		{"owner/repo/extra", "", "", true},
		{"owner/", "", "", true},
		{"/repo", "", "", true},
		{"noslash", "", "", true},
		// dot segments and invalid names
		{"../repo", "", "", true},
		{"owner/..", "", "", true},
		{"owner/.", "", "", true},
		{"own.er/repo", "", "", true},
		{"../etc/passwd", "", "", true},
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
	src := initRepoWithCommit(t)
	dest := filepath.Join(t.TempDir(), "clone.git")

	if err := CloneBare(context.Background(), src, dest); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "HEAD")); err != nil {
		t.Error("bare clone missing HEAD file")
	}
}

func TestCloneBareInvalidURL(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "clone.git")
	if err := CloneBare(context.Background(), "/nonexistent/repo", dest); err == nil {
		t.Fatal("expected error for invalid source")
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Parallel()
	bare := initBareRepo(t)
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch == "" {
		t.Error("branch is empty")
	}
}

func TestFetchOriginNoRemote(t *testing.T) {
	t.Parallel()
	bare := initBareRepo(t)
	err := FetchOrigin(context.Background(), bare)
	if err == nil {
		t.Fatal("expected error fetching from repo with no origin")
	}
}

// TestFetchOriginTTLSkipsRepeatFetch proves the FetchTTL cache added for
// issue #1527: within TTL, a second FetchOrigin call against the same bare
// clone must not see a commit pushed to origin after the first call — the
// second call is served from cache, not a real fetch. Not t.Parallel(): it
// mutates the package-level FetchTTL/fetchTTLNow globals and must not race
// other tests that call FetchOrigin expecting TTL disabled (the default).
func TestFetchOriginTTLSkipsRepeatFetch(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	now := time.Now()
	origTTL, origNow := FetchTTL, fetchTTLNow
	FetchTTL = 60 * time.Second
	fetchTTLNow = func() time.Time { return now }
	t.Cleanup(func() {
		FetchTTL, fetchTTLNow = origTTL, origNow
		lastFetchAt.Delete(filepath.Clean(bare))
	})

	trackingRef := "refs/remotes/origin/" + branch
	revParse := func() string {
		t.Helper()
		out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "rev-parse", "--verify", trackingRef).CombinedOutput()
		if err != nil {
			t.Fatalf("rev-parse %s: %v: %s", trackingRef, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("initial FetchOrigin: %v", err)
	}
	before := revParse()

	// Push a new commit to origin, then re-fetch a moment later (still well
	// inside the TTL window). The cached call must be a no-op.
	if out, err := exec.Command("git", "-C", src, "commit", "--allow-empty", "-m", "ttl-probe").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	now = now.Add(1 * time.Second)
	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("cached FetchOrigin: %v", err)
	}
	if afterCached := revParse(); afterCached != before {
		t.Fatalf("cached FetchOrigin call fetched anyway: tracking ref moved from %s to %s", before, afterCached)
	}

	// Once the TTL elapses, the next call must pick up the new commit.
	now = now.Add(60 * time.Second)
	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("post-TTL FetchOrigin: %v", err)
	}
	if afterExpired := revParse(); afterExpired == before {
		t.Fatal("post-TTL FetchOrigin did not refresh the tracking ref")
	}
}

func TestWorktreeHealthyAndRepair(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "worktree")
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test", branch); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if !WorktreeHealthy(context.Background(), wtPath) {
		t.Fatal("fresh worktree should be healthy")
	}

	// Simulate the synapse→sybra path-rename scenario: rewrite the .git
	// pointer to a path that no longer exists.
	dotGit := filepath.Join(wtPath, ".git")
	if err := os.WriteFile(dotGit, []byte("gitdir: /nonexistent/path/that/does/not/exist\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	if WorktreeHealthy(context.Background(), wtPath) {
		t.Fatal("broken worktree should not be healthy")
	}

	if err := RepairWorktrees(context.Background(), bare); err != nil {
		t.Fatalf("RepairWorktrees: %v", err)
	}
	if !WorktreeHealthy(context.Background(), wtPath) {
		t.Fatal("worktree should be healthy after repair")
	}
}

func TestCreateAndRemoveWorktree(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "worktree")
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test-task", branch); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Error("worktree missing README.md")
	}

	if err := RemoveWorktree(context.Background(), bare, wtPath); err != nil {
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
			name:    "conventional format slug-id",
			raw:     "worktree /tmp/wt\nHEAD abc1234567890\nbranch refs/heads/feat/implement-auth-a1b2c3d4\n",
			wantLen: 1, wantTaskID: "a1b2c3d4", wantBranch: "feat/implement-auth-a1b2c3d4",
		},
		{
			name:    "conventional non-sybra branch",
			raw:     "worktree /tmp/wt\nHEAD abc1234567890\nbranch refs/heads/feat/foo\n",
			wantLen: 1, wantTaskID: "", wantBranch: "feat/foo",
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

func TestAutoCommitUncommitted(t *testing.T) {
	t.Parallel()

	dir := initRepoWithCommit(t)

	if got := AutoCommitUncommitted(context.Background(), dir, "wip: nothing to do"); got {
		t.Fatal("expected no commit on a clean tree")
	}

	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := AutoCommitUncommitted(context.Background(), dir, "wip: recovered work"); !got {
		t.Fatal("expected a commit for a dirty tree")
	}

	statusOut, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("worktree still dirty after commit: %q", statusOut)
	}

	logOut, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := strings.TrimSpace(string(logOut)); got != "wip: recovered work" {
		t.Errorf("commit message = %q, want %q", got, "wip: recovered work")
	}

	bodyOut, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(bodyOut), "Signed-off-by: Sybra <sybra@localhost>") {
		t.Errorf("commit body missing DCO trailer, got %q", string(bodyOut))
	}

	// Idempotent: nothing left to commit on the now-clean tree.
	if got := AutoCommitUncommitted(context.Background(), dir, "wip: should not commit"); got {
		t.Fatal("expected no commit on an already-clean tree")
	}
}

func TestSanitizeWorktree_AbortsRebase(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	branch, _ := DefaultBranch(context.Background(), bare)
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test", branch); err != nil {
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

	if err := SanitizeWorktree(context.Background(), wtPath); err != nil {
		t.Fatalf("SanitizeWorktree: %v", err)
	}

	// Rebase should be aborted.
	statusOut, _ = exec.Command("git", "-C", wtPath, "status").Output()
	if contains(string(statusOut), "rebase") {
		t.Error("rebase still in progress after sanitize")
	}
}

func TestSanitizeWorktree_ClearsStaleRebaseStateWhenAbortFails(t *testing.T) {
	t.Parallel()

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	branch, _ := DefaultBranch(context.Background(), bare)
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test", branch); err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Simulate a stale/corrupt rebase-state directory that `git rebase
	// --abort` cannot clean up on its own (e.g. left behind by a killed
	// process): a rebase-merge dir with no valid onto/head-name files makes
	// `git rebase --abort` exit non-zero rather than actually aborting.
	stateDir := rebaseStateDir(context.Background(), wtPath)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Confirm the synthetic state actually defeats `git rebase --abort` on
	// its own, otherwise this test would pass even without the os.RemoveAll
	// fallback in clearRebaseState.
	abortCmd := exec.Command("git", "rebase", "--abort")
	abortCmd.Dir = wtPath
	if err := abortCmd.Run(); err == nil {
		t.Fatal("expected `git rebase --abort` to fail against the simulated stale state")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := SanitizeWorktree(context.Background(), wtPath); err != nil {
		t.Fatalf("SanitizeWorktree: %v", err)
	}

	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("expected stale rebase-state dir to be removed, stat err = %v", err)
	}
}

func TestSanitizeWorktree_DeletesShadowBranches(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	branch, _ := DefaultBranch(context.Background(), bare)
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test", branch); err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Create a local branch that shadows origin/main.
	cmd := exec.Command("git", "branch", "origin/main", "HEAD")
	cmd.Dir = wtPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create shadow branch: %v: %s", err, out)
	}

	if err := SanitizeWorktree(context.Background(), wtPath); err != nil {
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
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	branch, _ := DefaultBranch(context.Background(), bare)
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test", branch); err != nil {
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

	if err := SanitizeWorktree(context.Background(), wtPath); err != nil {
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

func TestCheckpointCommit(t *testing.T) {
	t.Parallel()

	t.Run("dirty tree commits", func(t *testing.T) {
		t.Parallel()
		repo := initRepoWithCommit(t)
		if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		committed, err := CheckpointCommit(context.Background(), repo, "chore(checkpoint): save progress")
		if err != nil {
			t.Fatalf("CheckpointCommit: %v", err)
		}
		if !committed {
			t.Fatal("CheckpointCommit reported committed=false on a dirty tree")
		}

		out, err := exec.Command("git", "-C", repo, "log", "--format=%s", "-1").Output()
		if err != nil {
			t.Fatalf("git log: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != "chore(checkpoint): save progress" {
			t.Fatalf("last subject = %q", got)
		}
		statusOut, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if strings.TrimSpace(string(statusOut)) != "" {
			t.Fatalf("worktree not clean after checkpoint commit: %s", statusOut)
		}
	})

	t.Run("clean tree is noop", func(t *testing.T) {
		t.Parallel()
		repo := initRepoWithCommit(t)

		headBefore, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("git rev-parse before: %v", err)
		}
		committed, err := CheckpointCommit(context.Background(), repo, "chore(checkpoint): save progress")
		if err != nil {
			t.Fatalf("CheckpointCommit: %v", err)
		}
		if committed {
			t.Fatal("CheckpointCommit reported committed=true on a clean tree")
		}
		headAfter, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("git rev-parse after: %v", err)
		}
		if !bytes.Equal(bytes.TrimSpace(headBefore), bytes.TrimSpace(headAfter)) {
			t.Fatal("HEAD changed on a clean-tree checkpoint")
		}
	})

	t.Run("commit failure returns error", func(t *testing.T) {
		t.Parallel()
		repo := initRepoWithCommit(t)
		if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		hooksDir := filepath.Join(repo, ".git", "hooks-fail")
		if err := os.MkdirAll(hooksDir, 0o755); err != nil {
			t.Fatal(err)
		}
		hook := filepath.Join(hooksDir, "pre-commit")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", repo, "config", "core.hooksPath", hooksDir).CombinedOutput(); err != nil {
			t.Fatalf("git config core.hooksPath: %v: %s", err, out)
		}

		committed, err := CheckpointCommit(context.Background(), repo, "chore(checkpoint): save progress")
		if err == nil {
			t.Fatal("CheckpointCommit error = nil, want hook failure")
		}
		if committed {
			t.Fatal("CheckpointCommit reported committed=true on hook failure")
		}
	})
}

func TestResetWorktreeForRetry_DiscardsPartialWorkAndKeepsIgnoredNotes(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	branch, _ := DefaultBranch(context.Background(), bare)
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test", branch); err != nil {
		t.Fatalf("worktree: %v", err)
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
	baselineOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse baseline: %v", err)
	}
	baseline := strings.TrimSpace(string(baselineOut))

	excludeOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		t.Fatalf("git-path exclude: %v", err)
	}
	excludePath := strings.TrimSpace(string(excludeOut))
	if err := os.WriteFile(excludePath, []byte("NOTES.md\n"), 0o644); err != nil {
		t.Fatalf("write exclude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "NOTES.md"), []byte("keep scratchpad"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "partial.go"), []byte("package partial\n"), 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	for _, args := range [][]string{
		{"add", "partial.go"},
		{"commit", "-m", "partial agent commit"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("# partial edit"), 0o644); err != nil {
		t.Fatalf("write dirty readme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "untracked.agent-dirty"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	if err := ResetWorktreeForRetry(context.Background(), wtPath, baseline); err != nil {
		t.Fatalf("ResetWorktreeForRetry: %v", err)
	}

	headOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse head: %v", err)
	}
	if got := strings.TrimSpace(string(headOut)); got != baseline {
		t.Fatalf("HEAD = %q, want baseline %q", got, baseline)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "partial.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial.go exists after reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "untracked.agent-dirty")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untracked.agent-dirty exists after clean: %v", err)
	}
	if notes, err := os.ReadFile(filepath.Join(wtPath, "NOTES.md")); err != nil || string(notes) != "keep scratchpad" {
		t.Fatalf("NOTES.md = %q, err %v; want preserved ignored scratchpad", notes, err)
	}
	statusOut, _ := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("expected clean tracked working tree, got: %s", statusOut)
	}
}

func TestCreateWorktreeInvalidBase(t *testing.T) {
	t.Parallel()
	bare := initBareRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	err := CreateWorktree(context.Background(), bare, wtPath, "test-branch", "nonexistent-base")
	if err == nil {
		t.Fatal("expected error for invalid base branch")
	}
}

func initWorktree(t *testing.T) (bare, wtPath string) {
	t.Helper()
	src := initRepoWithCommit(t)
	bare = filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	wtPath = filepath.Join(t.TempDir(), "wt")
	if err := CreateWorktree(context.Background(), bare, wtPath, "synapse/test", branch); err != nil {
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
		wantVerify    []string
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
		{
			name:       "verify only repo is non-nil",
			repo:       &ChecksConfig{Verify: []string{"go test ./..."}},
			wantVerify: []string{"go test ./..."},
		},
		{
			name:       "repo wins verify",
			repo:       &ChecksConfig{Verify: []string{"repo test"}},
			app:        &ChecksConfig{Verify: []string{"app test"}},
			wantVerify: []string{"repo test"},
		},
		{
			name:          "verify falls back to app",
			repo:          &ChecksConfig{PreCommit: []string{"echo repo"}},
			app:           &ChecksConfig{Verify: []string{"app test"}},
			wantPreCommit: []string{"echo repo"},
			wantVerify:    []string{"app test"},
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
			if !slicesEqual(got.Verify, tt.wantVerify) {
				t.Errorf("Verify = %v, want %v", got.Verify, tt.wantVerify)
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
	if err := os.WriteFile(filepath.Join(dir, ".sybra.yaml"), []byte(content), 0o644); err != nil {
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

func TestLoadRepoConfig_SetupBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := "setup:\n  - mise install\n  - (cd frontend && npm ci)\nchecks:\n  pre_commit:\n    - echo lint\n"
	if err := os.WriteFile(filepath.Join(dir, ".sybra.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatalf("LoadRepoConfig: %v", err)
	}
	if len(cfg.Setup) != 2 {
		t.Fatalf("Setup len = %d, want 2", len(cfg.Setup))
	}
	if cfg.Setup[0] != "mise install" {
		t.Errorf("Setup[0] = %q", cfg.Setup[0])
	}
	if cfg.Setup[1] != "(cd frontend && npm ci)" {
		t.Errorf("Setup[1] = %q", cfg.Setup[1])
	}
	// Sanity: checks block still parses when setup is present.
	if cfg.Checks == nil || len(cfg.Checks.PreCommit) != 1 {
		t.Errorf("checks dropped when setup added: %+v", cfg.Checks)
	}
}

func TestLoadRepoConfig_ManualTestBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := `manual_test:
  kind: server
  command: SYBRA_PORT=0 go run ./cmd/sybra-server
  health_url: http://127.0.0.1:$SYBRA_PORT/health
  probe_commands:
    - curl -fsS http://127.0.0.1:$SYBRA_PORT/api/tasks
    - sybra-cli --json list
`
	if err := os.WriteFile(filepath.Join(dir, ".sybra.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatalf("LoadRepoConfig: %v", err)
	}
	if cfg.ManualTest == nil {
		t.Fatal("ManualTest is nil")
	}
	if cfg.ManualTest.Kind != ManualTestKindServer {
		t.Errorf("ManualTest.Kind = %q, want %q", cfg.ManualTest.Kind, ManualTestKindServer)
	}
	if cfg.ManualTest.Command == "" || cfg.ManualTest.HealthURL == "" {
		t.Fatalf("ManualTest command/health missing: %+v", cfg.ManualTest)
	}
	if len(cfg.ManualTest.ProbeCommands) != 2 {
		t.Fatalf("ProbeCommands len = %d, want 2", len(cfg.ManualTest.ProbeCommands))
	}
}

func TestMergeSetup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		repo []string
		app  []string
		want []string
	}{
		{"both empty", nil, nil, nil},
		{"repo only", []string{"mise install"}, nil, []string{"mise install"}},
		{"app only", nil, []string{"cp .env.local .env"}, []string{"cp .env.local .env"}},
		{
			// Repo commands must run first so the canonical toolchain
			// bootstrap happens before any per-machine additions.
			name: "repo then app",
			repo: []string{"mise install", "npm ci"},
			app:  []string{"cp .env.local .env"},
			want: []string{"mise install", "npm ci", "cp .env.local .env"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeSetup(tt.repo, tt.app)
			if len(got) != len(tt.want) {
				t.Fatalf("MergeSetup = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("MergeSetup[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestMergeManualTest(t *testing.T) {
	t.Parallel()
	app := &ManualTestConfig{Kind: ManualTestKindCLI, Command: "sybra-cli list"}
	repo := &ManualTestConfig{Kind: ManualTestKindServer, Command: "go run ./cmd/sybra-server"}
	if got := MergeManualTest(nil, app); got != app {
		t.Fatalf("MergeManualTest(nil, app) = %+v, want app", got)
	}
	if got := MergeManualTest(repo, app); got != repo {
		t.Fatalf("MergeManualTest(repo, app) = %+v, want repo", got)
	}
}

func TestLoadRepoConfig_Invalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sybra.yaml"), []byte(":\n  bad: [yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRepoConfig(dir)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadRepoConfigAtDefaultBranch_Missing(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	cfg, err := LoadRepoConfigAtDefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil || cfg.Setup != nil {
		t.Errorf("expected empty RepoConfig, got %+v", cfg)
	}
}

// TestLoadRepoConfigAtDefaultBranch_IgnoresOtherBranches is the regression
// for issue #1519: a caller preparing an untrusted-ref worktree (a PR head,
// possibly from a fork, or a Renovate branch) must only ever see the
// .sybra.yaml tracked at the project's default branch, never a branch's own
// (potentially attacker-controlled) version of the file.
func TestLoadRepoConfigAtDefaultBranch_IgnoresOtherBranches(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	defaultBranch, err := CurrentBranch(context.Background(), src)
	if err != nil {
		t.Fatalf("current branch: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, ".sybra.yaml"), []byte("setup:\n  - touch trusted-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "trusted config")

	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}

	srcGit("checkout", "-b", "attacker-branch")
	if err := os.WriteFile(filepath.Join(src, ".sybra.yaml"), []byte("setup:\n  - touch evil-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "malicious config")
	srcGit("checkout", defaultBranch)

	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("fetch origin: %v", err)
	}

	cfg, err := LoadRepoConfigAtDefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("LoadRepoConfigAtDefaultBranch: %v", err)
	}
	if len(cfg.Setup) != 1 || cfg.Setup[0] != "touch trusted-marker" {
		t.Errorf("Setup = %v, want only the default-branch config", cfg.Setup)
	}
}

func TestInstallHooks_RepoConfigPriority(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)

	// Write .sybra.yaml with a failing pre-commit to prove repo config is used.
	repoYAML := "checks:\n  pre_commit:\n    - exit 1\n"
	if err := os.WriteFile(filepath.Join(wtPath, ".sybra.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// App config has a passing pre-commit — repo should win.
	appChecks := &ChecksConfig{PreCommit: []string{"exit 0"}}
	repoCfg, err := LoadRepoConfig(wtPath)
	if err != nil {
		t.Fatalf("LoadRepoConfig: %v", err)
	}
	merged := MergeChecks(repoCfg.Checks, appChecks)
	if err := InstallHooks(context.Background(), wtPath, merged); err != nil {
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
	_, wtPath := initWorktree(t)
	if err := InstallHooks(context.Background(), wtPath, nil); err != nil {
		t.Fatalf("InstallHooks(context.Background(), nil): %v", err)
	}
}

func TestInstallHooks_EmptySlices(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)
	if err := InstallHooks(context.Background(), wtPath, &ChecksConfig{}); err != nil {
		t.Fatalf("InstallHooks(context.Background(), empty): %v", err)
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
	_, wtPath := initWorktree(t)

	checks := &ChecksConfig{
		PreCommit: []string{"exit 1"},
	}
	if err := InstallHooks(context.Background(), wtPath, checks); err != nil {
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
	_, wtPath := initWorktree(t)

	checks := &ChecksConfig{
		PreCommit: []string{"exit 0"},
	}
	if err := InstallHooks(context.Background(), wtPath, checks); err != nil {
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
	_, wtPath := initWorktree(t)

	checks := &ChecksConfig{
		PrePush: []string{"echo pre-push ok"},
	}
	if err := InstallHooks(context.Background(), wtPath, checks); err != nil {
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
	_, wtPath := initWorktree(t)

	// Install first version.
	if err := InstallHooks(context.Background(), wtPath, &ChecksConfig{PreCommit: []string{"echo v1"}}); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Overwrite with second version.
	if err := InstallHooks(context.Background(), wtPath, &ChecksConfig{PreCommit: []string{"echo v2"}}); err != nil {
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

// TestCreateWorktree_PathExistsWithFiles covers a crashed-session recovery
// scenario: the worktree directory still contains leftover files from a
// previous run, but the `.git/worktrees/<name>/` admin dir is gone. Sybra
// calls CreateWorktree on the path, expecting a clean checkout. Git refuses
// because the destination is not empty — the error must propagate clearly so
// the caller can surface a "clean up stale path" hint rather than silently
// failing to start the agent.
func TestCreateWorktree_PathExistsWithFiles(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "stale-wt")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "leftover.txt"), []byte("crashed session debris"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = CreateWorktree(context.Background(), bare, wtPath, "sybra/stale-path", branch)
	if err == nil {
		t.Fatal("CreateWorktree into non-empty directory should error; got nil")
	}
	// The error text from `git worktree add` references the path; confirm
	// callers get actionable context rather than a generic exec failure.
	if !strings.Contains(err.Error(), wtPath) && !strings.Contains(err.Error(), "exists") {
		t.Errorf("error should reference the conflicting path or say 'exists'; got %v", err)
	}
}

// TestCreateWorktree_DuplicatePathRejected verifies that CreateWorktree
// refuses to overwrite an existing worktree. The original intent was to
// race two goroutines against the same destination path, but git's own
// locking around `worktree add` is best-effort — we observed both
// ref-lock failures ("update_ref failed: cannot lock ref HEAD") and
// "both succeeded but worktree is empty" across macOS and Linux CI
// runners. The real invariant — that the app layer doesn't swallow the
// second attempt — is captured without the race: create once, attempt
// again, assert the second call errors.
func TestCreateWorktree_DuplicatePathRejected(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "race-wt")

	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/first", branch); err != nil {
		t.Fatalf("first CreateWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Fatalf("first worktree missing README.md: %v", err)
	}

	// Second attempt with a different branch but the same target path must
	// fail — this is the guard against app-layer regressions that swallow
	// the error and leave phantom worktree metadata.
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/second", branch); err == nil {
		t.Errorf("second CreateWorktree on occupied path returned nil; expected error")
	}
}

func TestPushRemote_DefaultOrigin(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)
	if got := PushRemote(context.Background(), wtPath); got != "origin" {
		t.Errorf("PushRemote without fork = %q, want %q", got, "origin")
	}
}

func TestPushRemote_DetectsFork(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)

	forkBare := filepath.Join(t.TempDir(), "fork.git")
	if out, err := exec.Command("git", "init", "--bare", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("init fork bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", wtPath, "remote", "add", "fork", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("add fork remote: %v: %s", err, out)
	}

	if got := PushRemote(context.Background(), wtPath); got != "fork" {
		t.Errorf("PushRemote with fork = %q, want %q", got, "fork")
	}
}

func TestHeadArg_NoFork(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)
	got, err := HeadArg(context.Background(), wtPath, "my-branch")
	if err != nil {
		t.Fatalf("HeadArg: %v", err)
	}
	if got != "my-branch" {
		t.Errorf("HeadArg without fork = %q, want %q", got, "my-branch")
	}
}

func TestHeadArg_WithFork(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)
	if out, err := exec.Command("git", "-C", wtPath, "remote", "add", "fork", "git@github.com:someuser/widgets.git").CombinedOutput(); err != nil {
		t.Fatalf("add fork remote: %v: %s", err, out)
	}
	got, err := HeadArg(context.Background(), wtPath, "my-branch")
	if err != nil {
		t.Fatalf("HeadArg: %v", err)
	}
	if want := "someuser:my-branch"; got != want {
		t.Errorf("HeadArg with fork = %q, want %q", got, want)
	}
}

// TestPushUpstream_RoutesToFork verifies that PushUpstream targets the fork
// remote when one is configured — this is the core kuma-PR-from-fork fix.
// Without this routing, sybra's initial branch push lands on the upstream
// repo and gh pr create then opens a same-repo PR.
func TestPushUpstream_RoutesToFork(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)

	originBare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", originBare).CombinedOutput(); err != nil {
		t.Fatalf("init origin bare: %v: %s", err, out)
	}
	forkBare := filepath.Join(t.TempDir(), "fork.git")
	if out, err := exec.Command("git", "init", "--bare", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("init fork bare: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"remote", "set-url", "origin", originBare},
		{"remote", "add", "fork", forkBare},
		{"checkout", "-b", "sybra/route-test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := PushUpstream(context.Background(), wtPath, "sybra/route-test"); err != nil {
		t.Fatalf("PushUpstream: %v", err)
	}

	// Branch should exist on fork, not origin.
	forkOut, _ := exec.Command("git", "-c", "safe.bareRepository=all", "-C", forkBare, "branch", "--list", "sybra/route-test").Output()
	if strings.TrimSpace(string(forkOut)) == "" {
		t.Error("branch missing on fork after PushUpstream")
	}
	originOut, _ := exec.Command("git", "-c", "safe.bareRepository=all", "-C", originBare, "branch", "--list", "sybra/route-test").Output()
	if strings.TrimSpace(string(originOut)) != "" {
		t.Errorf("branch should not exist on origin; got %q", originOut)
	}
}

// TestEnforceForkOnlyPush_BlocksOriginPush proves the transport-level guard:
// when a fork remote exists, an agent's `git push origin <branch>` fails
// before the network call, regardless of --no-verify. This is the deterministic
// floor that backs up the prompt-level guidance pointing agents at fork.
func TestEnforceForkOnlyPush_BlocksOriginPush(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)

	originBare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", originBare).CombinedOutput(); err != nil {
		t.Fatalf("init origin bare: %v: %s", err, out)
	}
	forkBare := filepath.Join(t.TempDir(), "fork.git")
	if out, err := exec.Command("git", "init", "--bare", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("init fork bare: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"remote", "set-url", "origin", originBare},
		{"remote", "add", "fork", forkBare},
		{"checkout", "-b", "fix/route-test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := EnforceForkOnlyPush(context.Background(), wtPath); err != nil {
		t.Fatalf("EnforceForkOnlyPush: %v", err)
	}

	// Push to origin must fail. --no-verify ensures we're testing the
	// transport-level guard, not a pre-push hook.
	pushCmd := exec.Command("git", "push", "--no-verify", "origin", "fix/route-test")
	pushCmd.Dir = wtPath
	out, err := pushCmd.CombinedOutput()
	if err == nil {
		t.Fatalf("push to origin should fail when fork remote exists; got success: %s", out)
	}
	if !strings.Contains(string(out), forkOnlyDisabledPushURL) {
		t.Errorf("push error should reference sentinel pushurl so the cause is obvious; got: %s", out)
	}

	// Push to fork must still work.
	pushFork := exec.Command("git", "push", "fork", "fix/route-test")
	pushFork.Dir = wtPath
	if out, err := pushFork.CombinedOutput(); err != nil {
		t.Fatalf("push to fork should succeed: %v: %s", err, out)
	}
}

// TestEnforceForkOnlyPush_NoForkLeavesOriginPushable confirms the guard is
// dormant on single-remote repos (pet projects without a fork) — pushing to
// origin must keep working.
func TestEnforceForkOnlyPush_NoForkLeavesOriginPushable(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)

	originBare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", originBare).CombinedOutput(); err != nil {
		t.Fatalf("init origin bare: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"remote", "set-url", "origin", originBare},
		{"checkout", "-b", "feat/route-test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := EnforceForkOnlyPush(context.Background(), wtPath); err != nil {
		t.Fatalf("EnforceForkOnlyPush: %v", err)
	}

	pushCmd := exec.Command("git", "push", "origin", "feat/route-test")
	pushCmd.Dir = wtPath
	if out, err := pushCmd.CombinedOutput(); err != nil {
		t.Fatalf("push to origin should succeed without a fork remote: %v: %s", err, out)
	}
}

// TestEnforceForkOnlyPush_RestoresAfterForkRemoved verifies the sentinel
// pushurl is cleared when the fork remote disappears, so removing the fork
// reverts the worktree to normal origin pushes. Foreign pushurl values set
// by the user are left untouched.
func TestEnforceForkOnlyPush_RestoresAfterForkRemoved(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)

	originBare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", originBare).CombinedOutput(); err != nil {
		t.Fatalf("init origin bare: %v: %s", err, out)
	}
	forkBare := filepath.Join(t.TempDir(), "fork.git")
	if out, err := exec.Command("git", "init", "--bare", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("init fork bare: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"remote", "set-url", "origin", originBare},
		{"remote", "add", "fork", forkBare},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := EnforceForkOnlyPush(context.Background(), wtPath); err != nil {
		t.Fatalf("EnforceForkOnlyPush (with fork): %v", err)
	}

	rm := exec.Command("git", "remote", "remove", "fork")
	rm.Dir = wtPath
	if out, err := rm.CombinedOutput(); err != nil {
		t.Fatalf("remove fork: %v: %s", err, out)
	}

	if err := EnforceForkOnlyPush(context.Background(), wtPath); err != nil {
		t.Fatalf("EnforceForkOnlyPush (after fork removed): %v", err)
	}

	got, _ := exec.Command("git", "-C", wtPath, "config", "--get", "remote.origin.pushurl").Output()
	if trimmed := strings.TrimSpace(string(got)); trimmed != "" {
		t.Errorf("pushurl should be cleared after fork remote removed; got %q", trimmed)
	}
}

// TestEnforceForkOnlyPush_PreservesForeignPushURL ensures the guard does not
// trample a user-set pushurl when no fork remote exists. We only own the
// sentinel value.
func TestEnforceForkOnlyPush_PreservesForeignPushURL(t *testing.T) {
	t.Parallel()
	_, wtPath := initWorktree(t)

	foreignURL := "https://user.example.com/custom-push-url.git"
	for _, args := range [][]string{
		{"remote", "set-url", "--push", "origin", foreignURL},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if err := EnforceForkOnlyPush(context.Background(), wtPath); err != nil {
		t.Fatalf("EnforceForkOnlyPush: %v", err)
	}

	got, _ := exec.Command("git", "-C", wtPath, "config", "--get", "remote.origin.pushurl").Output()
	if trimmed := strings.TrimSpace(string(got)); trimmed != foreignURL {
		t.Errorf("user pushurl clobbered; got %q want %q", trimmed, foreignURL)
	}
}

// TestListWorktrees_OrphanedAdminDir covers the recovery mismatch where a
// user manually rm -rfs the working tree directory but leaves the
// `.git/worktrees/<name>/` admin entry. `git worktree list` still reports the
// orphan. Sybra's ListWorktrees passes through this output — downstream code
// must be prepared to stat-check each returned path and prune those that are
// missing on disk. The test pins the current semantics so a regression that
// silently drops (or crashes on) orphaned entries is visible.
func TestListWorktrees_OrphanedAdminDir(t *testing.T) {
	t.Parallel()
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "orphan-wt")
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/orphan", branch); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Nuke the working tree directory without informing git. The admin dir
	// under bare/.git/worktrees/orphan-wt remains in place.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatal(err)
	}

	wts, err := ListWorktrees(context.Background(), bare)
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}

	// Admin entry still present — git lists the missing path. Callers must
	// stat-check. After PruneWorktrees the orphan is gone.
	found := false
	for _, wt := range wts {
		if wt.Path == wtPath {
			found = true
			if _, statErr := os.Stat(wt.Path); statErr == nil {
				t.Errorf("orphan path %s still exists on disk; test setup failed", wt.Path)
			}
		}
	}
	if !found {
		t.Logf("git already pruned orphan entry (version-dependent); this is acceptable")
	}

	// PruneWorktrees must succeed and leave no orphan entry behind.
	if err := PruneWorktrees(context.Background(), bare); err != nil {
		t.Fatalf("PruneWorktrees: %v", err)
	}
	wts2, err := ListWorktrees(context.Background(), bare)
	if err != nil {
		t.Fatalf("ListWorktrees after prune: %v", err)
	}
	for _, wt := range wts2 {
		if wt.Path == wtPath {
			t.Errorf("orphan %s still listed after prune", wt.Path)
		}
	}
}

// setupPushSyncWorktree builds: bare "remote" ← bare sybra clone ← worktree.
// The worktree pushes against the remote bare, which accepts force pushes
// and lets us inspect ref state to verify PushSync's mode selection.
func setupPushSyncWorktree(t *testing.T) (remoteBare, wtPath, wtBranch string) {
	t.Helper()
	src := initRepoWithCommit(t)
	remoteBare = filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", remoteBare).CombinedOutput(); err != nil {
		t.Fatalf("init remote bare: %v: %s", err, out)
	}
	// Enable reflog on the bare so we can count actual ref updates per branch.
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", remoteBare, "config", "core.logAllRefUpdates", "true").CombinedOutput(); err != nil {
		t.Fatalf("config logAllRefUpdates: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"-C", src, "remote", "add", "rem", remoteBare},
		{"-C", src, "push", "rem", "HEAD"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	sybraBare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), remoteBare, sybraBare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), sybraBare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	wtPath = filepath.Join(t.TempDir(), "wt")
	wtBranch = "sybra/push-test"
	if err := CreateWorktree(context.Background(), sybraBare, wtPath, wtBranch, branch); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return remoteBare, wtPath, wtBranch
}

// makeCommit writes a file and creates a commit in wtPath. Returns the new HEAD SHA.
func makeCommit(t *testing.T, wtPath, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wtPath, "data.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "change: " + content},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// remoteRefSHA returns the SHA the remote bare resolves for branch, or "" if absent.
func remoteRefSHA(t *testing.T, remoteBare, branch string) string {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.bareRepository=all", "-C", remoteBare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// remoteReflogCount returns the number of reflog entries for branch on the remote bare.
func remoteReflogCount(t *testing.T, remoteBare, branch string) int {
	t.Helper()
	cmd := exec.Command("git", "-c", "safe.bareRepository=all", "-C", remoteBare, "reflog", "show", "refs/heads/"+branch)
	out, _ := cmd.Output() // empty output is fine when the ref has no entries yet
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

func TestPushSync_BranchMissing(t *testing.T) {
	t.Parallel()
	_, wtPath, _ := setupPushSyncWorktree(t)
	if err := PushSync(context.Background(), wtPath, "no-such-branch"); !errors.Is(err, ErrBranchMissing) {
		t.Fatalf("PushSync missing branch: got %v, want ErrBranchMissing", err)
	}
}

func TestPushSync_FirstPushSetsTracking(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	localSHA := makeCommit(t, wtPath, "first")

	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync first push: %v", err)
	}
	if got := remoteRefSHA(t, remoteBare, branch); got != localSHA {
		t.Fatalf("remote SHA after first push = %q, want %q", got, localSHA)
	}
}

func TestPushSync_NoopWhenSynced(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync first: %v", err)
	}
	entriesBefore := remoteReflogCount(t, remoteBare, branch)
	if entriesBefore == 0 {
		t.Fatalf("first push left no reflog entry (logAllRefUpdates may be off)")
	}

	// Second sync with no local changes must not touch the remote.
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync second (no-op): %v", err)
	}
	if got := remoteReflogCount(t, remoteBare, branch); got != entriesBefore {
		t.Fatalf("remote reflog grew from %d to %d on a no-op sync (a push was issued)", entriesBefore, got)
	}
}

func TestPushSync_FastForwardWithoutForce(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	// Reject force-pushes on the remote so a fast-forward must succeed without force.
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", remoteBare, "config", "receive.denyNonFastForwards", "true").CombinedOutput(); err != nil {
		t.Fatalf("denyNonFastForwards: %v: %s", err, out)
	}

	newSHA := makeCommit(t, wtPath, "two")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync fast-forward: %v", err)
	}
	if got := remoteRefSHA(t, remoteBare, branch); got != newSHA {
		t.Fatalf("remote SHA after fast-forward = %q, want %q", got, newSHA)
	}
}

// TestPushSync_DivergenceReturnsErrorNoForce guards the core "never
// force-push" property: on a genuinely diverged branch, PushSync must refuse
// to push (returning ErrDivergedNeedsResolve) rather than force-with-lease
// over remote-only commits.
func TestPushSync_DivergenceReturnsErrorNoForce(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	makeCommit(t, wtPath, "two")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}
	beforeSHA := remoteRefSHA(t, remoteBare, branch)

	// Rewrite history locally so HEAD diverges from the remote tracking ref.
	if out, err := exec.Command("git", "-C", wtPath, "reset", "--hard", "HEAD~1").CombinedOutput(); err != nil {
		t.Fatalf("reset: %v: %s", err, out)
	}
	makeCommit(t, wtPath, "two-prime")

	// Prove a regular push would now be rejected — confirms this is a genuine
	// divergence, not a fast-forward PushSync mis-detected as one.
	rejectCmd := exec.Command("git", "push", "origin", branch)
	rejectCmd.Dir = wtPath
	if out, err := rejectCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected regular push to be rejected on divergence; succeeded: %s", out)
	}

	err := PushSync(context.Background(), wtPath, branch)
	if !errors.Is(err, ErrDivergedNeedsResolve) {
		t.Fatalf("PushSync divergence = %v, want ErrDivergedNeedsResolve", err)
	}
	if got := remoteRefSHA(t, remoteBare, branch); got != beforeSHA {
		t.Fatalf("remote SHA = %q, want untouched %q (PushSync must never force-push)", got, beforeSHA)
	}
}

// pushRemoteCommit simulates a fix pushed to branch from another clone/machine:
// it clones remoteBare, commits on branch, pushes, and returns the new SHA. This
// leaves the caller's worktree local ref stale relative to the remote head.
func pushRemoteCommit(t *testing.T, remoteBare, branch, content string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	if out, err := exec.Command("git", "clone", "-b", branch, remoteBare, other).CombinedOutput(); err != nil {
		t.Fatalf("clone other: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"config", "user.email", "other@test.com"},
		{"config", "user.name", "Other"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = other
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	sha := makeCommit(t, other, content)
	push := exec.Command("git", "push", "origin", "HEAD:"+branch)
	push.Dir = other
	if out, err := push.CombinedOutput(); err != nil {
		t.Fatalf("push other: %v: %s", err, out)
	}
	return sha
}

// TestReconcileWithRemote_FastForwardsStaleLocal is the regression for the
// force-push data-loss bug: a fix pushed to the PR branch from another clone
// must be adopted into a reused worktree's stale local branch before rebase, so
// it is not dropped by a later force-push.
func TestReconcileWithRemote_FastForwardsStaleLocal(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	// Another clone pushes a fix; wtPath's local branch is now stale.
	fixSHA := pushRemoteCommit(t, remoteBare, branch, "review-fix")

	if err := ReconcileWithRemote(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("ReconcileWithRemote: %v", err)
	}

	headCmd := exec.Command("git", "rev-parse", "HEAD")
	headCmd.Dir = wtPath
	headOut, err := headCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	head := strings.TrimSpace(string(headOut))
	if head != fixSHA {
		t.Fatalf("local HEAD after reconcile = %q, want fix SHA %q (fast-forward failed)", head, fixSHA)
	}
}

// TestReconcileWithRemote_DivergedReturnsError guards against clobbering a
// remote that has genuinely diverged: reconcile must refuse rather than let a
// later force-push silently overwrite remote-only commits.
func TestReconcileWithRemote_DivergedReturnsError(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	// Remote advances one way; local advances a different, incompatible way.
	pushRemoteCommit(t, remoteBare, branch, "remote-side")
	makeCommit(t, wtPath, "local-side")

	err := ReconcileWithRemote(context.Background(), wtPath, branch)
	if !errors.Is(err, ErrBranchDiverged) {
		t.Fatalf("ReconcileWithRemote diverged = %v, want ErrBranchDiverged", err)
	}
}

// TestReconcileWithRemote_PropagatesFetchError guards the fail-closed fix: a
// fetch failure other than the expected "couldn't find remote ref" (first
// push) case must propagate instead of letting reconcile silently no-op on
// stale history.
func TestReconcileWithRemote_PropagatesFetchError(t *testing.T) {
	t.Parallel()
	_, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	// Point origin at an unreachable path so fetch fails for a reason other
	// than a missing remote ref.
	if out, err := exec.Command("git", "-C", wtPath, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "no-such-remote.git")).CombinedOutput(); err != nil {
		t.Fatalf("remote set-url: %v: %s", err, out)
	}

	if err := ReconcileWithRemote(context.Background(), wtPath, branch); err == nil {
		t.Fatal("ReconcileWithRemote with broken remote = nil error, want propagated fetch failure")
	}
}

// TestReconcileWithRemote_DirtyWorktreeFailsClosed guards the ready-pr/manual
// status-flip path: if another actor left uncommitted edits in the worktree,
// reconcile must not fast-forward HEAD under those live changes.
func TestReconcileWithRemote_DirtyWorktreeFailsClosed(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	fixSHA := pushRemoteCommit(t, remoteBare, branch, "review-fix")

	if err := os.WriteFile(filepath.Join(wtPath, "local.txt"), []byte("still editing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := ReconcileWithRemote(context.Background(), wtPath, branch)
	if !errors.Is(err, ErrDirtyWorktree) {
		t.Fatalf("ReconcileWithRemote dirty = %v, want ErrDirtyWorktree", err)
	}

	headOut, headErr := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if headErr != nil {
		t.Fatalf("rev-parse HEAD: %v", headErr)
	}
	head := strings.TrimSpace(string(headOut))
	if head == fixSHA {
		t.Fatalf("local HEAD advanced to remote fix SHA %q despite dirty worktree", fixSHA)
	}

	statusOut, statusErr := exec.Command("git", "-C", wtPath, "status", "--porcelain").CombinedOutput()
	if statusErr != nil {
		t.Fatalf("git status: %v: %s", statusErr, statusOut)
	}
	if !strings.Contains(string(statusOut), "local.txt") {
		t.Fatalf("dirty file missing after failed reconcile; status = %q", statusOut)
	}
}

// TestMergeOnto_MergesNonConflictingHistories proves the core "additive, no
// force-push" property: merging two branches whose commits touch different
// files produces a merge commit carrying both sides' content, and pushing the
// result afterward is a plain fast-forward (the remote SHA remains an
// ancestor of the merge commit).
func TestMergeOnto_MergesNonConflictingHistories(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}
	remoteHead := pushRemoteCommit(t, remoteBare, branch, "remote-side")

	// A local commit on a different file — nothing for the merge to conflict
	// on with remote-side's data.txt change.
	if err := os.WriteFile(filepath.Join(wtPath, "other.txt"), []byte("local-side"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", wtPath, "add", "other.txt"},
		{"-C", wtPath, "commit", "-m", "local-side"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	if out, err := exec.Command("git", "-C", wtPath, "fetch", "origin").CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v: %s", err, out)
	}
	if err := MergeOnto(context.Background(), wtPath, "origin/"+branch); err != nil {
		t.Fatalf("MergeOnto: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(wtPath, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "remote-side" {
		t.Fatalf("data.txt = %q, want remote-side's content", got)
	}
	got, err = os.ReadFile(filepath.Join(wtPath, "other.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "local-side" {
		t.Fatalf("other.txt = %q, want local-side's content", got)
	}

	// The remote's pre-merge head must remain an ancestor of the merge commit
	// — the property PushSync relies on to always take the fast-forward path,
	// never --force-with-lease, after a MergeOnto recovery.
	if out, err := exec.Command("git", "-C", wtPath, "merge-base", "--is-ancestor", remoteHead, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("remote head %s is not an ancestor of the merge commit: %v: %s", remoteHead, err, out)
	}
}

// TestMergeOnto_ConflictReturnsErrorAndCleansUp proves MergeOnto fails closed
// on a genuine conflict and leaves the worktree in a clean, non-merging state
// (mirroring RebaseOnto's --abort-on-failure contract) rather than stranding
// a half-applied merge for the next operation to trip over.
func TestMergeOnto_ConflictReturnsErrorAndCleansUp(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}
	pushRemoteCommit(t, remoteBare, branch, "remote-side")
	makeCommit(t, wtPath, "local-side")

	if out, err := exec.Command("git", "-C", wtPath, "fetch", "origin").CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v: %s", err, out)
	}

	err := MergeOnto(context.Background(), wtPath, "origin/"+branch)
	if err == nil {
		t.Fatal("MergeOnto with conflicting data.txt = nil error, want conflict")
	}

	if out, mergeErr := exec.Command("git", "-C", wtPath, "rev-parse", "-q", "--verify", "MERGE_HEAD").CombinedOutput(); mergeErr == nil {
		t.Fatalf("MERGE_HEAD still present after MergeOnto failure, want abort to have cleaned it up: %s", out)
	}
	out, statusErr := exec.Command("git", "-C", wtPath, "status", "--porcelain").CombinedOutput()
	if statusErr != nil {
		t.Fatalf("git status: %v: %s", statusErr, out)
	}
	if len(strings.TrimSpace(string(out))) != 0 {
		t.Fatalf("worktree not clean after MergeOnto failure: %s", out)
	}
}

// TestPushSync_RefusesForceWhenRemoteAdvanced is the defense-in-depth net: when
// another clone has pushed to the branch since this worktree last synced,
// PushSync must refresh its view of the remote (rather than compare against
// its own stale tracking ref) and refuse to push at all instead of clobbering
// the newer commits with a force push.
func TestPushSync_RefusesForceWhenRemoteAdvanced(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	makeCommit(t, wtPath, "two")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	// Diverge local from the tracking ref so PushSync takes the force path.
	if out, err := exec.Command("git", "-C", wtPath, "reset", "--hard", "HEAD~1").CombinedOutput(); err != nil {
		t.Fatalf("reset: %v: %s", err, out)
	}
	makeCommit(t, wtPath, "two-prime")

	// Meanwhile the remote branch advances from another clone. wtPath never
	// explicitly fetches, so its cached tracking ref stays behind the live
	// remote head until PushSync's own fetch refreshes it.
	advancedSHA := pushRemoteCommit(t, remoteBare, branch, "concurrent-fix")

	err := PushSync(context.Background(), wtPath, branch)
	if !errors.Is(err, ErrDivergedNeedsResolve) {
		t.Fatalf("PushSync = %v, want ErrDivergedNeedsResolve", err)
	}
	if !errors.Is(err, ErrRemoteAdvanced) {
		t.Fatalf("PushSync = %v, want ErrRemoteAdvanced", err)
	}
	// The remote fix must survive untouched.
	if got := remoteRefSHA(t, remoteBare, branch); got != advancedSHA {
		t.Fatalf("remote SHA = %q, want untouched fix %q", got, advancedSHA)
	}
}

// TestPushSync_RefreshesStaleCacheBeforeComparing is the regression for the
// create_pr/branch-conflict-fix tight loop (issue #1628): a stale cached
// refs/remotes/<remote>/<branch> can point at a commit that is not an
// ancestor of the true (live) remote head — e.g. left over from a prior
// divergence — even though the local branch and the live remote head are
// actually identical right now (a separate recovery path already converged
// them). Comparing against the stale cache alone would misreport this as an
// unresolved divergence forever, since the "true" state never gets a chance
// to update the cache; PushSync must fetch fresh before deciding.
func TestPushSync_RefreshesStaleCacheBeforeComparing(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	sha1 := makeCommit(t, wtPath, "one")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	// Legitimately advance and push — this becomes the true, converged state
	// both locally and on the remote.
	shaX := makeCommit(t, wtPath, "two-a")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync advance: %v", err)
	}

	// Build a sibling commit off sha1 purely to obtain a real, valid SHA that
	// is not an ancestor of shaX — simulating a stale/incorrect cached ref.
	if out, err := exec.Command("git", "-C", wtPath, "reset", "--hard", sha1).CombinedOutput(); err != nil {
		t.Fatalf("reset to sha1: %v: %s", err, out)
	}
	shaY := makeCommit(t, wtPath, "two-b")
	if out, err := exec.Command("git", "-C", wtPath, "merge-base", "--is-ancestor", shaY, shaX).CombinedOutput(); err == nil {
		t.Fatalf("shaY unexpectedly an ancestor of shaX: %s", out)
	}

	// Restore local to the true converged state (shaX) but leave the cached
	// tracking ref corrupted at the unrelated sibling (shaY) — this is the
	// "stale cache" the fix must not trust blindly.
	if out, err := exec.Command("git", "-C", wtPath, "reset", "--hard", shaX).CombinedOutput(); err != nil {
		t.Fatalf("reset to shaX: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", wtPath, "update-ref", "refs/remotes/origin/"+branch, shaY).CombinedOutput(); err != nil {
		t.Fatalf("corrupt cached tracking ref: %v: %s", err, out)
	}

	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync = %v, want nil (converged after fresh fetch)", err)
	}
	if got := remoteRefSHA(t, remoteBare, branch); got != shaX {
		t.Fatalf("remote SHA = %q, want unchanged converged state %q", got, shaX)
	}
}

// TestPushSync_FailsClosedWhenRemoteHeadUnverifiable guards the fail-closed
// fix: if the live remote head can't be verified before a push, PushSync
// must refuse rather than proceed with a force push against unconfirmed
// remote state.
func TestPushSync_FailsClosedWhenRemoteHeadUnverifiable(t *testing.T) {
	t.Parallel()
	remoteBare, wtPath, branch := setupPushSyncWorktree(t)
	makeCommit(t, wtPath, "one")
	makeCommit(t, wtPath, "two")
	if err := PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}
	beforeSHA := remoteRefSHA(t, remoteBare, branch)

	// Diverge local from the tracking ref so PushSync takes the force path.
	if out, err := exec.Command("git", "-C", wtPath, "reset", "--hard", "HEAD~1").CombinedOutput(); err != nil {
		t.Fatalf("reset: %v: %s", err, out)
	}
	makeCommit(t, wtPath, "two-prime")

	// Break the remote so the live-head verification fetch errors instead of
	// updating the tracking ref.
	if out, err := exec.Command("git", "-C", wtPath, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "no-such-remote.git")).CombinedOutput(); err != nil {
		t.Fatalf("remote set-url: %v: %s", err, out)
	}

	err := PushSync(context.Background(), wtPath, branch)
	if !errors.Is(err, ErrRemoteAdvanced) {
		t.Fatalf("PushSync = %v, want ErrRemoteAdvanced (fail closed)", err)
	}
	if errors.Is(err, ErrDivergedNeedsResolve) {
		t.Fatalf("PushSync = %v, must not return ErrDivergedNeedsResolve for remote verification failure", err)
	}
	if got := remoteRefSHA(t, remoteBare, branch); got != beforeSHA {
		t.Fatalf("remote SHA = %q, want untouched %q", got, beforeSHA)
	}
}

func TestFetchPRHead(t *testing.T) {
	t.Parallel()
	// Build an upstream whose PR-42 head commit is reachable ONLY via
	// refs/pull/42/head — never as a normal branch head. This mirrors a fork
	// PR: the head branch lives in the fork, but GitHub still publishes the
	// head at refs/pull/<N>/head on the upstream.
	upstream := initRepoWithCommit(t)
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", upstream}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	defBranch, err := exec.Command("git", "-C", upstream, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("default branch: %v: %s", err, defBranch)
	}
	run("checkout", "-b", "fork-feature")
	if err := os.WriteFile(filepath.Join(upstream, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "fork feature")
	shaOut, err := exec.Command("git", "-C", upstream, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, shaOut)
	}
	prSHA := strings.TrimSpace(string(shaOut))
	run("update-ref", "refs/pull/42/head", prSHA)
	run("checkout", strings.TrimSpace(string(defBranch)))
	run("branch", "-D", "fork-feature")

	// A plain bare clone copies heads only — not refs/pull/* — so the PR
	// commit is absent until FetchPRHead pulls it on demand.
	bare := filepath.Join(t.TempDir(), "clone.git")
	if err := CloneBare(context.Background(), upstream, bare); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	ref, err := FetchPRHead(context.Background(), bare, 42)
	if err != nil {
		t.Fatalf("FetchPRHead: %v", err)
	}
	if ref != "refs/sybra/pr/42" {
		t.Errorf("ref = %q, want refs/sybra/pr/42", ref)
	}
	got, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "rev-parse", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v: %s", ref, err, got)
	}
	if strings.TrimSpace(string(got)) != prSHA {
		t.Errorf("fetched ref at %q, want %q", strings.TrimSpace(string(got)), prSHA)
	}

	// The detached worktree that PrepareForReview creates must succeed from
	// the fetched PR head — the original failure mode.
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := CreateWorktreeDetached(context.Background(), bare, wtPath, ref); err != nil {
		t.Fatalf("CreateWorktreeDetached from PR head: %v", err)
	}
}

func TestListTrackedFiles(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", src},
		{"git", "-C", src, "config", "user.email", "test@test.com"},
		{"git", "-C", src, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "internal", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "internal", "foo", "bar.go"), []byte("package foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", src, "add", "."},
		{"git", "-C", src, "commit", "-m", "init"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	branch, err := exec.Command("git", "-C", src, "branch", "--show-current").CombinedOutput()
	if err != nil {
		t.Fatalf("branch --show-current: %v: %s", err, branch)
	}
	branchName := strings.TrimSpace(string(branch))

	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	t.Run("tracked files", func(t *testing.T) {
		t.Parallel()
		files, err := ListTrackedFiles(context.Background(), bare, "refs/heads/"+branchName)
		if err != nil {
			t.Fatalf("ListTrackedFiles: %v", err)
		}
		want := map[string]bool{"internal/foo/bar.go": true, "README.md": true}
		if len(files) != len(want) {
			t.Fatalf("files = %v, want %v", files, want)
		}
		for _, f := range files {
			if !want[f] {
				t.Errorf("unexpected file %q", f)
			}
		}
	})

	t.Run("empty tree", func(t *testing.T) {
		t.Parallel()
		empty := initBareRepo(t)
		// The well-known empty-tree SHA1 is valid in every git repo without
		// needing any commits.
		files, err := ListTrackedFiles(context.Background(), empty, "4b825dc642cb6eb9a060e54bf8d69288fbee4904")
		if err != nil {
			t.Fatalf("ListTrackedFiles on empty tree: %v", err)
		}
		if files == nil {
			t.Error("files is nil, want non-nil empty slice")
		}
		if len(files) != 0 {
			t.Errorf("files = %v, want empty", files)
		}
	})

	t.Run("missing ref", func(t *testing.T) {
		t.Parallel()
		if _, err := ListTrackedFiles(context.Background(), bare, "refs/heads/does-not-exist"); err == nil {
			t.Fatal("expected error for missing ref")
		}
	})
}

func TestTrackedFilesAtDefaultBranch(t *testing.T) {
	t.Parallel()
	t.Run("falls back to local head before first fetch", func(t *testing.T) {
		t.Parallel()
		src := initRepoWithCommit(t)
		bare := filepath.Join(t.TempDir(), "bare.git")
		if err := CloneBare(context.Background(), src, bare); err != nil {
			t.Fatalf("CloneBare: %v", err)
		}
		// git clone --bare only populates refs/heads/*, not
		// refs/remotes/origin/* (that requires a subsequent fetch), so this
		// must resolve via the refs/heads/<branch> fallback.
		files, err := TrackedFilesAtDefaultBranch(context.Background(), bare)
		if err != nil {
			t.Fatalf("TrackedFilesAtDefaultBranch: %v", err)
		}
		if len(files) != 1 || files[0] != "README.md" {
			t.Fatalf("files = %v, want [README.md]", files)
		}
	})

	t.Run("prefers remote-tracking ref once fetched", func(t *testing.T) {
		t.Parallel()
		src := initRepoWithCommit(t)
		bare := filepath.Join(t.TempDir(), "bare.git")
		if err := CloneBare(context.Background(), src, bare); err != nil {
			t.Fatalf("CloneBare: %v", err)
		}
		branch, err := DefaultBranch(context.Background(), bare)
		if err != nil {
			t.Fatalf("DefaultBranch: %v", err)
		}

		// Add a file upstream and fetch it into the bare clone, but leave
		// the local refs/heads/<branch> stale — simulates a bare clone
		// that's been fetched without its local head being advanced.
		if err := os.WriteFile(filepath.Join(src, "new.txt"), []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"git", "-C", src, "add", "."},
			{"git", "-C", src, "commit", "-m", "add new.txt"},
		} {
			if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
				t.Fatalf("%v: %v: %s", args, err, out)
			}
		}
		if err := FetchOrigin(context.Background(), bare); err != nil {
			t.Fatalf("FetchOrigin: %v", err)
		}

		files, err := TrackedFilesAtDefaultBranch(context.Background(), bare)
		if err != nil {
			t.Fatalf("TrackedFilesAtDefaultBranch: %v", err)
		}
		want := map[string]bool{"README.md": true, "new.txt": true}
		if len(files) != len(want) {
			t.Fatalf("files = %v, want %v", files, want)
		}
		for _, f := range files {
			if !want[f] {
				t.Errorf("unexpected file %q", f)
			}
		}
		// The stale local head must not have been consulted.
		staleFiles, err := ListTrackedFiles(context.Background(), bare, "refs/heads/"+branch)
		if err != nil {
			t.Fatalf("ListTrackedFiles on stale head: %v", err)
		}
		if len(staleFiles) != 1 {
			t.Fatalf("expected stale local head to remain at 1 file, got %v", staleFiles)
		}
	})
}

// initSecondWorktree adds a second worktree from the same bare repo, checked
// out on branch (created off baseBranch when it doesn't already exist as a
// local branch), so a commit made there is immediately visible to sibling
// worktrees via the shared git dir — no push required.
func initSecondWorktree(t *testing.T, bare, branch, baseBranch string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "wt2")
	var err error
	if BranchExists(context.Background(), bare, branch) {
		err = CreateWorktreeExisting(context.Background(), bare, dir, branch)
	} else {
		err = CreateWorktree(context.Background(), bare, dir, branch, baseBranch)
	}
	if err != nil {
		t.Fatalf("create second worktree: %v", err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func writeAndCommit(t *testing.T, dir, file, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", message},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestTryCleanMerge(t *testing.T) {
	t.Parallel()

	t.Run("created: clean merge advances the branch", func(t *testing.T) {
		t.Parallel()
		bare, wtPath := initWorktree(t)
		branch, err := DefaultBranch(context.Background(), bare)
		if err != nil {
			t.Fatalf("DefaultBranch: %v", err)
		}

		// Advance base with an unrelated file in a sibling worktree.
		basePath := initSecondWorktree(t, bare, branch, branch)
		writeAndCommit(t, basePath, "unrelated.txt", "unrelated change", "advance base")

		preHEAD, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse pre HEAD: %v", err)
		}

		result, err := TryCleanMerge(context.Background(), wtPath, "refs/heads/"+branch)
		if err != nil {
			t.Fatalf("TryCleanMerge: %v", err)
		}
		if result != CleanMergeCreated {
			t.Fatalf("result = %v, want CleanMergeCreated", result)
		}

		postHEAD, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse post HEAD: %v", err)
		}
		if bytes.Equal(postHEAD, preHEAD) {
			t.Fatal("HEAD did not move after a created clean merge")
		}
		if _, err := os.Stat(filepath.Join(wtPath, "unrelated.txt")); err != nil {
			t.Errorf("merged file missing after clean merge: %v", err)
		}
		statusOut, _ := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
		if strings.TrimSpace(string(statusOut)) != "" {
			t.Errorf("worktree not clean after created merge: %s", statusOut)
		}
	})

	t.Run("conflict: worktree is left clean", func(t *testing.T) {
		t.Parallel()
		bare, wtPath := initWorktree(t)
		branch, err := DefaultBranch(context.Background(), bare)
		if err != nil {
			t.Fatalf("DefaultBranch: %v", err)
		}

		// Conflicting edits to README.md on both sides.
		writeAndCommit(t, wtPath, "README.md", "task branch change", "task edit")
		basePath := initSecondWorktree(t, bare, branch, branch)
		writeAndCommit(t, basePath, "README.md", "base branch change", "base edit")

		preHEAD, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse pre HEAD: %v", err)
		}

		result, err := TryCleanMerge(context.Background(), wtPath, "refs/heads/"+branch)
		if err != nil {
			t.Fatalf("TryCleanMerge: %v", err)
		}
		if result != CleanMergeConflict {
			t.Fatalf("result = %v, want CleanMergeConflict", result)
		}

		postHEAD, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse post HEAD: %v", err)
		}
		if !bytes.Equal(postHEAD, preHEAD) {
			t.Error("HEAD moved after a conflicting merge, want unchanged")
		}
		statusOut, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if strings.TrimSpace(string(statusOut)) != "" {
			t.Fatalf("worktree not clean after conflicting merge: %s", statusOut)
		}
		if _, err := os.Stat(filepath.Join(wtPath, ".git", "MERGE_HEAD")); err == nil {
			t.Error("MERGE_HEAD still present after conflict cleanup")
		}
	})

	t.Run("no-op: branch already contains base", func(t *testing.T) {
		t.Parallel()
		bare, wtPath := initWorktree(t)
		branch, err := DefaultBranch(context.Background(), bare)
		if err != nil {
			t.Fatalf("DefaultBranch: %v", err)
		}

		preHEAD, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse pre HEAD: %v", err)
		}

		result, err := TryCleanMerge(context.Background(), wtPath, "refs/heads/"+branch)
		if err != nil {
			t.Fatalf("TryCleanMerge: %v", err)
		}
		if result != CleanMergeNoop {
			t.Fatalf("result = %v, want CleanMergeNoop", result)
		}

		postHEAD, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse post HEAD: %v", err)
		}
		if !bytes.Equal(postHEAD, preHEAD) {
			t.Error("HEAD moved on a no-op merge")
		}
	})

	t.Run("invalid base ref returns an error", func(t *testing.T) {
		t.Parallel()
		_, wtPath := initWorktree(t)

		result, err := TryCleanMerge(context.Background(), wtPath, "refs/heads/does-not-exist")
		if err == nil {
			t.Fatal("expected error for unresolvable base ref")
		}
		if result != CleanMergeConflict {
			t.Errorf("result = %v, want CleanMergeConflict on error", result)
		}
	})

	t.Run("fatal merge failure returns an error instead of looking like a conflict", func(t *testing.T) {
		t.Parallel()
		bare, wtPath := initWorktree(t)
		branch, err := DefaultBranch(context.Background(), bare)
		if err != nil {
			t.Fatalf("DefaultBranch: %v", err)
		}

		basePath := initSecondWorktree(t, bare, branch, branch)
		writeAndCommit(t, basePath, "unrelated.txt", "unrelated change", "advance base")

		gitDirOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "--git-dir").CombinedOutput()
		if err != nil {
			t.Fatalf("rev-parse --git-dir: %v: %s", err, gitDirOut)
		}
		gitDir := strings.TrimSpace(string(gitDirOut))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(wtPath, gitDir)
		}
		mergeHeadPath := filepath.Join(gitDir, "MERGE_HEAD")
		if err := os.WriteFile(mergeHeadPath, []byte("deadbeef\n"), 0o644); err != nil {
			t.Fatalf("write MERGE_HEAD: %v", err)
		}
		defer os.Remove(mergeHeadPath)

		result, err := TryCleanMerge(context.Background(), wtPath, "refs/heads/"+branch)
		if err == nil {
			t.Fatal("expected fatal git merge failure to return an error")
		}
		if result != CleanMergeConflict {
			t.Fatalf("result = %v, want CleanMergeConflict on fatal merge error", result)
		}
		if !strings.Contains(err.Error(), "MERGE_HEAD") && !strings.Contains(err.Error(), "merge") {
			t.Fatalf("error = %v, want merge-state context", err)
		}

		head, headErr := exec.Command("git", "-C", wtPath, "rev-parse", "--verify", "HEAD").CombinedOutput()
		if headErr != nil {
			t.Fatalf("rev-parse HEAD after fatal merge failure: %v: %s", headErr, head)
		}
		statusOut, statusErr := exec.Command("git", "-C", wtPath, "status", "--porcelain").CombinedOutput()
		if statusErr != nil {
			t.Fatalf("git status after fatal merge failure: %v: %s", statusErr, statusOut)
		}
		if strings.TrimSpace(string(statusOut)) != "" {
			t.Fatalf("worktree not clean after fatal merge failure: %s", statusOut)
		}
	})
}

func TestInstallSignoffHook(t *testing.T) {
	t.Parallel()

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "worktree")
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test", branch); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	gitWt := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", wtPath}, args...)
		if out, gerr := exec.Command("git", full...).CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v: %s", args, gerr, out)
		}
	}
	gitWt("config", "user.email", "agent@example.com")
	gitWt("config", "user.name", "Agent")

	if err := InstallSignoffHook(context.Background(), wtPath); err != nil {
		t.Fatalf("InstallSignoffHook: %v", err)
	}

	body := func() string {
		t.Helper()
		out, gerr := exec.Command("git", "-C", wtPath, "log", "-1", "--format=%B").Output()
		if gerr != nil {
			t.Fatalf("git log: %v", gerr)
		}
		return string(out)
	}

	wantSOB := "Signed-off-by: Agent <agent@example.com>"

	if err := os.WriteFile(filepath.Join(wtPath, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitWt("add", ".")
	gitWt("commit", "-m", "feat: add a")
	if got := body(); !strings.Contains(got, wantSOB) {
		t.Errorf("plain commit missing DCO trailer, got:\n%s", got)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitWt("add", ".")
	gitWt("commit", "-s", "-m", "feat: add b")
	if got := body(); strings.Count(got, wantSOB) != 1 {
		t.Errorf("commit -s should not duplicate the trailer, got %d in:\n%s",
			strings.Count(got, wantSOB), got)
	}
}

func TestInstallSignoffHook_OverridesHooksPath(t *testing.T) {
	t.Parallel()

	dir := initRepoWithCommit(t)
	gitDir := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	stray := filepath.Join(t.TempDir(), "stray-hooks")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	gitDir("config", "core.hooksPath", stray)

	if err := InstallSignoffHook(context.Background(), dir); err != nil {
		t.Fatalf("InstallSignoffHook: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDir("add", ".")
	gitDir("commit", "-m", "feat: x")

	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(out), "Signed-off-by: Test <test@test.com>") {
		t.Errorf("hook did not run despite a stray core.hooksPath override, got:\n%s", out)
	}
}

func TestCloneBare_InstallsSignoffHook(t *testing.T) {
	t.Parallel()

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "worktree")
	if err := CreateWorktree(context.Background(), bare, wtPath, "sybra/test", branch); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	gitWt := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", wtPath}, args...)
		if out, gerr := exec.Command("git", full...).CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v: %s", args, gerr, out)
		}
	}
	gitWt("config", "user.email", "agent@example.com")
	gitWt("config", "user.name", "Agent")

	if err := os.WriteFile(filepath.Join(wtPath, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitWt("add", ".")
	gitWt("commit", "-m", "feat: add c")

	out, err := exec.Command("git", "-C", wtPath, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if want := "Signed-off-by: Agent <agent@example.com>"; !strings.Contains(string(out), want) {
		t.Errorf("CloneBare worktree commit missing DCO trailer, got:\n%s", out)
	}
}

func TestStripHTTPSUserinfo(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{"token userinfo", "https://ghp_abc123@github.com/o/r.git", "https://github.com/o/r.git", true},
		{"user colon token", "https://user:ghp_x@github.com/o/r.git", "https://github.com/o/r.git", true},
		{"clean https", "https://github.com/o/r.git", "https://github.com/o/r.git", false},
		{"ssh untouched", "git@github.com:o/r.git", "git@github.com:o/r.git", false},
		{"at in path only", "https://github.com/o/r@v2.git", "https://github.com/o/r@v2.git", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := stripHTTPSUserinfo(tc.in)
			if got != tc.want || changed != tc.changed {
				t.Errorf("stripHTTPSUserinfo(%q) = (%q,%v), want (%q,%v)", tc.in, got, changed, tc.want, tc.changed)
			}
		})
	}
}
