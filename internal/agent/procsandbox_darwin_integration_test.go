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

// TestSandboxEnforce_GCAndPackRefsSucceeds reproduces the housekeeping gap
// found in a second-pass review of the fetch/merge/add/commit fix above: an
// explicit `git gc` (and the `git pack-refs --all` it runs internally) also
// touches packed-refs(.new/.lock), gc.pid(.lock), the branch's own reflog
// lock, and regenerates info/refs via a randomly-suffixed xmkstemp() temp
// file — none of which the fetch/merge/commit path above exercises. Every
// one of those needed its own grant; this failed with "Operation not
// permitted" at each step until all were added.
//
// Scope: this is the solo-task case — no sibling worktree shares the bare
// clone. See TestSandboxEnforce_GCDegradesGracefullyWithSiblingWorktree for
// the concurrent-task case, where `--all` maintenance touches refs this
// task's own grant deliberately does not extend to.
func TestSandboxEnforce_GCAndPackRefsSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "cd " + wt + " && git fetch origin 2>&1 && git gc 2>&1 && git pack-refs --all 2>&1; echo EXIT=$?"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "Operation not permitted") {
		t.Errorf("git gc/pack-refs hit EPERM under sandbox: %s", got)
	}
	if !strings.Contains(got, "EXIT=0") {
		t.Errorf("git gc/pack-refs did not exit cleanly: %s", got)
	}
}

// TestSandboxEnforce_GCDegradesGracefullyWithSiblingWorktree documents an
// accepted limitation, not a bug: `git gc`'s internal `git reflog expire
// --all` and `git pack-refs --all --prune` iterate every worktree sharing
// the bare clone, not just the caller's own, so with a sibling worktree
// present they try to lock/prune the sibling's ref and reflog — which this
// task's grant deliberately does not extend to (see
// GIT_BRANCH_REF_FILE/_LOCK_FILE's literal-not-subpath doc above). That
// isolation is the whole point of the narrow, per-task grant, so widening
// it to make `--all` fully succeed would defeat the property
// TestSandboxEnforce_SiblingBranchRefFileIsolated (and
// TestSandboxEnforce_GitAdminDirIsolatedFromSiblingWorktree) exist to prove.
// git itself treats each maintenance sub-step's failure as non-fatal: `gc`
// still exits 0, having completed the housekeeping it *can* do for the
// caller's own state, and simply logs (does not propagate) the refs it
// could not touch. This test locks in that non-fatal, isolation-preserving
// degradation — an EPERM in the output here is expected and correct, not a
// regression to chase with another grant.
func TestSandboxEnforce_GCDegradesGracefullyWithSiblingWorktree(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	bare, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	siblingWt := filepath.Join(filepath.Dir(wt), "sibling-wt")
	runGitOrFatal(t, bare, "worktree", "add", siblingWt, "-b", "fix/sibling-branch", "main")

	script := "cd " + wt + " && git fetch origin 2>&1 && git gc 2>&1; echo EXIT=$?"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if !strings.Contains(got, "EXIT=0") {
		t.Fatalf("git gc must exit 0 even when it cannot touch a sibling's refs: %s", got)
	}

	log := exec.Command("git", "log", "--format=%s", "-1")
	log.Dir = wt
	logOut, logErr := log.CombinedOutput()
	if logErr != nil {
		t.Errorf("this task's own repo state must remain intact after gc: %v: %s", logErr, logOut)
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

// TestSandboxEnforce_SiblingBranchRefFileIsolated proves the branch-ref
// literal grant does not widen into a sibling task's own branch ref, even
// when both branches nest under the same parent path segment (both live
// under refs/heads/fix/) — the isolation property GIT_BRANCH_REF_FILE's
// literal-not-subpath design (see agent_sandbox.sb) exists to guarantee.
func TestSandboxEnforce_SiblingBranchRefFileIsolated(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	bare, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	runGitOrFatal(t, bare, "branch", "fix/sibling-branch", "main")
	siblingRefFile := filepath.Join(bare, "refs", "heads", "fix", "sibling-branch")

	script := "(echo tampered > " + siblingRefFile + " 2>/dev/null && echo LEAK) || echo DENIED"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "LEAK") || !strings.Contains(got, "DENIED") {
		t.Errorf("write to a sibling branch's own ref file must be kernel-denied: %q", got)
	}
}

// TestSandboxEnforce_InfoDirDeniesAttributesAndExclude proves the
// GIT_INFO_DIR subpath grant does not widen into info/attributes or
// info/exclude — hand-authored, behavior-altering config shared with every
// sibling task on the clone, unlike the idempotent regenerated info/refs
// and info/packs the grant exists for.
func TestSandboxEnforce_InfoDirDeniesAttributesAndExclude(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "COMMON=$(cd " + wt + " && git rev-parse --git-common-dir); " +
		"(echo evil > \"$COMMON/info/attributes\" 2>/dev/null && echo LEAK_ATTR) || echo DENIED_ATTR; " +
		"(echo evil >> \"$COMMON/info/exclude\" 2>/dev/null && echo LEAK_EXCLUDE) || echo DENIED_EXCLUDE; " +
		"(echo x > \"$COMMON/info/refs\" 2>/dev/null && echo ALLOWED_REFS) || echo DENIED_REFS"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "LEAK_ATTR") || !strings.Contains(got, "DENIED_ATTR") {
		t.Errorf("write to info/attributes must be kernel-denied: %q", got)
	}
	if strings.Contains(got, "LEAK_EXCLUDE") || !strings.Contains(got, "DENIED_EXCLUDE") {
		t.Errorf("write to info/exclude must be kernel-denied: %q", got)
	}
	if !strings.Contains(got, "ALLOWED_REFS") {
		t.Errorf("write to info/refs must still succeed (the grant this deny carves out of): %q", got)
	}
}

// TestSandboxEnforce_ShallowFetchSucceeds covers a git operation not issued
// by Sybra's own git calls today but reachable if an agent runs one
// directly: a shallow fetch locks gitCommonDir/shallow the same way a
// normal fetch locks packed-refs.
func TestSandboxEnforce_ShallowFetchSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "cd " + wt + " && git fetch --depth=1 origin main 2>&1; echo EXIT=$?"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "Operation not permitted") {
		t.Errorf("shallow fetch hit EPERM under sandbox: %s", got)
	}
	if !strings.Contains(got, "EXIT=0") {
		t.Errorf("shallow fetch did not exit cleanly: %s", got)
	}
}

// TestSandboxEnforce_GitStashSucceeds reproduces a gap found by an
// independent adversarial manual-test pass: refs/stash is a single,
// fixed-name ref directly under refs/, outside every ref-dir/tag-dir/
// remote-dir grant above and outside the branch's own literal grant (which
// only covers that branch's own ref, not a repo-wide stash stack) — a very
// plausible mid-task operation (stash local edits before a rebase/pull)
// that failed with "Cannot save the current status" before this grant.
func TestSandboxEnforce_GitStashSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "cd " + wt + " && echo base > f.txt && git add f.txt && git commit -q -m tracked && " +
		"echo modified >> f.txt && git stash push -m mystash 2>&1 && git stash pop 2>&1; echo EXIT=$?"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "Operation not permitted") {
		t.Errorf("git stash hit EPERM under sandbox: %s", got)
	}
	if !strings.Contains(got, "EXIT=0") {
		t.Errorf("git stash did not exit cleanly: %s", got)
	}
}

// TestSandboxEnforce_GitNotesSucceeds reproduces a second gap found by the
// same adversarial pass: refs/notes/* is a repo-wide annotation namespace
// (like remotes/tags, not a task's own exclusive branch work), outside
// every grant above — `git notes add` failed with "cannot lock ref
// 'refs/notes/commits': unable to create directory" before this grant.
func TestSandboxEnforce_GitNotesSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, wt := setupLinkedWorktree(t)
	cfg := buildEnforceCfg(t, wt)

	script := "cd " + wt + " && git notes add -m 'note body' 2>&1; echo EXIT=$?"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, _ := cmd.CombinedOutput()
	got := string(out)

	if strings.Contains(got, "Operation not permitted") {
		t.Errorf("git notes hit EPERM under sandbox: %s", got)
	}
	if !strings.Contains(got, "EXIT=0") {
		t.Errorf("git notes did not exit cleanly: %s", got)
	}
}
