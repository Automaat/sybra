//go:build darwin

package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGitOrFatal runs a git command in dir, unsandboxed, for test setup.
func runGitOrFatal(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v (dir=%s): %v: %s", args, dir, err, out)
	}
}

// setupLinkedWorktree builds a bare clone + linked worktree pair matching a
// real Sybra task layout: a shared bare clone (analogous to
// ~/.sybra/clones/<owner>/<repo>.git) with a linked worktree checked out from
// it on its own branch, and the standard tracking refspec CloneBare
// configures so `git fetch origin` actually updates refs/remotes/origin/*.
func setupLinkedWorktree(t *testing.T) (bare, worktree string) {
	t.Helper()
	src := t.TempDir()
	runGitOrFatal(t, src, "init", "-q", "-b", "main")
	runGitOrFatal(t, src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")

	bare = filepath.Join(t.TempDir(), "bare.git")
	runGitOrFatal(t, "", "clone", "-q", "--bare", src, bare)
	runGitOrFatal(t, bare, "config", "user.email", "t@t")
	runGitOrFatal(t, bare, "config", "user.name", "t")
	runGitOrFatal(t, bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")

	worktree = filepath.Join(t.TempDir(), "wt")
	runGitOrFatal(t, bare, "worktree", "add", worktree, "-b", "fix/task-branch", "main")

	// Advance upstream after the worktree is created, so fetch+merge below
	// has real forward progress to make, not a no-op.
	runGitOrFatal(t, src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "upstream change")
	return bare, worktree
}

// buildEnforceCfg drives the exact production discovery pipeline
// (resolveGitSandboxRoots → enforceSpec) that Manager.buildEnforceSpec uses,
// rather than hand-constructing a sandboxSpec — a hand-built spec can grant
// paths a real run would never compute the same way, silently passing a test
// that a production run would fail.
func buildEnforceCfg(t *testing.T, worktree string) *RunConfig {
	t.Helper()
	profilePath, err := materializeSandboxProfile()
	if err != nil {
		t.Fatalf("materializeSandboxProfile: %v", err)
	}
	wtCanon, err := canonicalizeRoot(worktree)
	if err != nil {
		t.Fatalf("canonicalizeRoot(worktree): %v", err)
	}
	gitRoots, err := resolveGitSandboxRoots(context.Background(), wtCanon)
	if err != nil {
		t.Fatalf("resolveGitSandboxRoots: %v", err)
	}
	spec := enforceSpec(wtCanon, nil, wtCanon, wtCanon, wtCanon, profilePath, "", gitRoots, gitSandboxOverlay{})
	return &RunConfig{sandbox: spec}
}

// TestSandboxEnforce_FullGitWorkflowSucceeds reproduces task 24849431's
// EPERM end to end: fetch, fast-forward merge, add, and commit — the exact
// operations the task's own diagnosis named — run for real under
// sandbox-exec, through the same discovery pipeline (resolveGitSandboxRoots,
// enforceSpec, newProviderCmd → wrapInvocation) a production dispatch uses.
// A linked worktree's gitdir (HEAD/index/logs/HEAD) and its branch's own
// ref/reflog live outside WORKTREE, under the shared bare clone; without
// these grants git fails partway through with the reported EPERM.
func TestSandboxEnforce_FullGitWorkflowSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "set -e; cd " + wt + "\n" +
		"git fetch origin\n" +
		"git merge --ff-only refs/remotes/origin/main\n" +
		"echo hello > f.txt\n" +
		"git add f.txt\n" +
		"git commit -q -m 'task change'\n" +
		"echo WORKFLOW_OK"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	got := string(out)

	if err != nil {
		t.Fatalf("fetch+merge+add+commit failed under sandbox (err=%v): %s", err, got)
	}
	if !strings.Contains(got, "WORKFLOW_OK") {
		t.Errorf("workflow did not reach its final step: %s", got)
	}

	log := exec.Command("git", "log", "--format=%s", "-1")
	log.Dir = wt
	logOut, logErr := log.CombinedOutput()
	if logErr != nil || strings.TrimSpace(string(logOut)) != "task change" {
		t.Errorf("commit did not land as expected (err=%v): %s", logErr, logOut)
	}
}

// TestSandboxEnforce_GitAdminDirIsolatedFromSiblingWorktree proves the
// per-task grant does not widen into a sibling task's own worktree admin
// dir sharing the same bare clone — the isolation property the narrow,
// per-task scoping depends on.
func TestSandboxEnforce_GitAdminDirIsolatedFromSiblingWorktree(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	bare, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	siblingWt := filepath.Join(filepath.Dir(wt), "sibling-wt")
	runGitOrFatal(t, bare, "worktree", "add", siblingWt, "-b", "fix/sibling-branch", "main")
	siblingAdminDir := filepath.Join(bare, "worktrees", "sibling-wt")

	script := "(touch " + filepath.Join(siblingAdminDir, "HEAD") + " 2>/dev/null && echo LEAK) || echo DENIED"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "LEAK") || !strings.Contains(got, "DENIED") {
		t.Errorf("write to a sibling worktree's admin dir must be kernel-denied: %q", got)
	}
}
