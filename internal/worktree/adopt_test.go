package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/notes"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func signoffHookPath(t *testing.T, wtPath string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", wtPath, "rev-parse", "--git-common-dir").CombinedOutput()
	if err != nil {
		t.Fatalf("resolve git common dir: %v: %s", err, out)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(wtPath, gitDir)
	}
	return filepath.Join(gitDir, "hooks", "prepare-commit-msg")
}

// TestPrepareForTask_AdoptsExternalWorktree verifies that a task carrying an
// explicit WorktreeDir is run in that directory verbatim: PrepareForTask
// returns the external path, records its branch, drops the identity beacon,
// creates nothing under the managed worktrees dir, and Remove leaves it alone.
func TestPrepareForTask_AdoptsExternalWorktree(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	// Simulate an Orca-created worktree: a real linked worktree off the bare
	// clone, checked out on its own branch.
	ext := filepath.Join(t.TempDir(), "orca-worktree")
	const extBranch = "orca/feature-x"
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", h.proj.ClonePath, "worktree", "add", "-b", extBranch, ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	tk, err := h.tasks.Create("adopt me", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id":   h.proj.ID,
		"worktree_dir": ext,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	got, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}
	if got != ext {
		t.Fatalf("adopted path = %q, want %q", got, ext)
	}

	// Beacon written into the adopted dir.
	if _, err := os.Stat(filepath.Join(ext, contextFileName)); err != nil {
		t.Errorf("context beacon not written: %v", err)
	}

	// Branch recorded from the adopted worktree's checkout.
	reloaded, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if reloaded.Branch != extBranch {
		t.Errorf("task branch = %q, want %q", reloaded.Branch, extBranch)
	}

	// Nothing created under the managed worktrees dir.
	entries, err := os.ReadDir(h.wtDir)
	if err != nil {
		t.Fatalf("read worktrees dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("managed worktrees dir not empty: %v", entries)
	}

	// Remove must not delete an adopted worktree.
	h.m.Remove(context.Background(), tk.ID)
	if _, err := os.Stat(ext); err != nil {
		t.Errorf("adopted worktree removed by cleanup: %v", err)
	}
}

// TestPrepareForFix_AdoptsExternalWorktree is the regression for the
// circuit-breaker strand: a handoff/adopted worktree sent through the PR-fix
// flow must be reused as-is, not re-created with `git worktree add` (which
// fails "already exists" and, after wtFailureLimit retries, flips the task to
// human-required). The adoption guard short-circuits before any PR-branch
// fetch, so the PR number is irrelevant here.
func TestPrepareForFix_AdoptsExternalWorktree(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	ext := filepath.Join(t.TempDir(), "orca-worktree")
	const extBranch = "orca/fix-x"
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", h.proj.ClonePath, "worktree", "add", "-b", extBranch, ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	tk, err := h.tasks.Create("adopt fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id":   h.proj.ID,
		"worktree_dir": ext,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	got, err := h.m.PrepareForFix(context.Background(), tk, 1)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}
	if got != ext {
		t.Fatalf("adopted path = %q, want %q", got, ext)
	}

	// Assert the adoption side effects so the test fails if adoption is ever
	// replaced by a plain early return: the identity beacon is written into the
	// adopted dir and the task records the adopted worktree's branch.
	if _, err := os.Stat(filepath.Join(ext, contextFileName)); err != nil {
		t.Errorf("context beacon not written: %v", err)
	}
	reloaded, err := h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if reloaded.Branch != extBranch {
		t.Errorf("task branch = %q, want %q", reloaded.Branch, extBranch)
	}

	// No managed worktree was created — proves no `git worktree add` ran.
	entries, err := os.ReadDir(h.wtDir)
	if err != nil {
		t.Fatalf("read worktrees dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("managed worktrees dir not empty: %v", entries)
	}
}

// TestPrepareForFix_FallsBackToPRHead is the regression for "fix review
// comments" failing with `fatal: invalid reference:
// refs/remotes/origin/<branch>`. When the PR head is not reachable under
// refs/remotes/origin/* (a fork PR, or a branch FetchOrigin could not pull),
// PrepareForFix must fall back to the refs/pull/<N>/head ref and still create a
// real local branch to push.
func TestPrepareForFix_FallsBackToPRHead(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	const prNumber = 7
	const branch = "default-delta-xds"

	// Build a commit in the origin remote (src) that is reachable ONLY via
	// refs/pull/<N>/head — never as a branch head. Mirrors a fork PR.
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "pr head")
	shaOut, err := exec.Command("git", "-C", h.src, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, shaOut)
	}
	prSHA := strings.TrimSpace(string(shaOut))
	srcGit("update-ref", "refs/pull/7/head", prSHA)
	srcGit("checkout", "main")
	srcGit("branch", "-D", branch)

	// Resolver returns the PR branch name; the head is not on origin.
	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("fix fork pr", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}

	// Worktree is on a real local branch (not detached) at the PR head commit.
	headBranch, err := exec.Command("git", "-C", wtPath, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD branch: %v: %s", err, headBranch)
	}
	if got := strings.TrimSpace(string(headBranch)); got != branch {
		t.Errorf("worktree branch = %q, want %q", got, branch)
	}
	headSHA, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v: %s", err, headSHA)
	}
	if got := strings.TrimSpace(string(headSHA)); got != prSHA {
		t.Errorf("worktree HEAD = %q, want PR head %q", got, prSHA)
	}
}

// TestPrepareForFix_ReusesHealthyWorktree is the regression for issue #1527:
// PrepareForFix used to unconditionally RemoveWorktreeReconcile + recreate
// the fix worktree on every dispatch, re-running setup (mise install, npm
// ci, npm run build:desktop) even when nothing about the worktree needed to
// change. A second dispatch against a healthy worktree already on the fix
// branch must reuse it — skipping setup entirely — while still picking up
// any commit pushed to the branch since the first dispatch.
func TestPrepareForFix_ReusesHealthyWorktree(t *testing.T) {
	h := prepareHarness(t, []string{"sh -c 'echo run >> setup-count.txt'"}, 0)

	const prNumber = 11
	const branch = "fix/reuse-me"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// setup-count.txt is a byproduct of the setup command, not agent work —
	// gitignore it so SanitizeWorktree's auto-commit-uncommitted-work step
	// doesn't turn it into a local-only commit that diverges from the next
	// remote push and defeats reuse.
	if err := os.WriteFile(filepath.Join(h.src, ".gitignore"), []byte("setup-count.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "gitignore setup byproduct")
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "initial fix commit")
	srcGit("checkout", "main")

	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("reuse fix worktree", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix (initial): %v", err)
	}
	setupCountPath := filepath.Join(wtPath, "setup-count.txt")
	countAfterFirst, err := os.ReadFile(setupCountPath)
	if err != nil {
		t.Fatalf("read setup-count.txt: %v", err)
	}
	if got := strings.Count(string(countAfterFirst), "run"); got != 1 {
		t.Fatalf("setup ran %d times on initial prepare, want 1", got)
	}
	hookPath := signoffHookPath(t, wtPath)
	if err := os.Remove(hookPath); err != nil {
		t.Fatalf("remove signoff hook: %v", err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "config", "--unset", "core.hooksPath").CombinedOutput(); err != nil {
		t.Fatalf("unset core.hooksPath: %v: %s", err, out)
	}

	// A second commit lands on the fix branch (e.g. pushed from another
	// clone) before the next dispatch.
	srcGit("checkout", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature2.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "second fix commit")
	srcGit("checkout", "main")

	got, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix (reuse): %v", err)
	}
	if got != wtPath {
		t.Fatalf("reused path = %q, want the same worktree %q", got, wtPath)
	}

	// Setup must not have re-run.
	countAfterSecond, err := os.ReadFile(setupCountPath)
	if err != nil {
		t.Fatalf("read setup-count.txt after reuse: %v", err)
	}
	if got := strings.Count(string(countAfterSecond), "run"); got != 1 {
		t.Fatalf("setup ran %d times across both prepares, want 1 (setup must be skipped on reuse)", got)
	}

	// The worktree must be fast-forwarded to the new remote commit.
	if _, err := os.Stat(filepath.Join(got, "feature2.txt")); err != nil {
		t.Errorf("reused worktree missing feature2.txt from the second remote commit: %v", err)
	}
	if _, err := os.Stat(hookPath); err != nil {
		t.Errorf("signoff hook was not restored on reuse: %v", err)
	}
}

// TestPrepareForFix_SetupFailureDoesNotBlock is the regression test for
// issue #1454: a project whose setup: command fails (e.g. the PR under
// repair broke the build) must not prevent PrepareForFix from creating the
// fix worktree — gating on that exact breakage would deadlock the task, since
// the fixer this worktree is for exists specifically to repair it.
func TestPrepareForFix_SetupFailureDoesNotBlock(t *testing.T) {
	h := prepareHarness(t, []string{"exit 1"}, 0)

	const prNumber = 9
	const branch = "fix-broken-build"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "pr commit")
	srcGit("checkout", "main")

	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("fix broken build", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix must succeed despite a failing setup command: %v", err)
	}

	content, readErr := os.ReadFile(filepath.Join(wtPath, notes.FileName))
	if readErr != nil {
		t.Fatalf("read scratchpad: %v", readErr)
	}
	if !strings.Contains(string(content), "Setup failure") {
		t.Errorf("scratchpad missing setup failure note: %q", content)
	}
}

// TestPrepareForFix_ReconcilesOrphanWorktreeDir is the regression for the
// human-required strand described in issue 1373 / task be14dfd3: a fix
// worktree directory survives on disk (e.g. `worktree prune`, or a crash
// mid-cleanup, dropped the admin entry under the bare repo's worktrees/ dir)
// while the checkout dir remains. `git worktree remove --force` alone fails
// on that orphan ("not a git repository (null)"), the error used to be
// swallowed, and every retry of PrepareForFix hit `already exists` until the
// circuit breaker escalated to human-required — for a failure mode that is
// mechanically self-healing. PrepareForFix must reconcile the orphan and
// succeed instead.
func TestPrepareForFix_ReconcilesOrphanWorktreeDir(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	const branch = "fix-branch"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "fix.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "fix commit")

	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("orphan fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForFix(context.Background(), tk, 1)
	if err != nil {
		t.Fatalf("PrepareForFix (initial): %v", err)
	}

	// Simulate the orphan: drop the bare repo's admin entry for this worktree
	// while leaving the checkout directory on disk. The .git file inside a
	// linked worktree points at its admin dir via "gitdir: <bare>/worktrees/<name>".
	gitFile, err := os.ReadFile(filepath.Join(wtPath, ".git"))
	if err != nil {
		t.Fatalf("read .git file: %v", err)
	}
	adminDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(gitFile)), "gitdir:"))
	if adminDir == "" {
		t.Fatalf("could not parse admin dir from .git file: %q", gitFile)
	}
	if !filepath.IsAbs(adminDir) {
		adminDir = filepath.Join(wtPath, adminDir)
	}
	if err := os.RemoveAll(adminDir); err != nil {
		t.Fatalf("remove admin dir: %v", err)
	}
	if project.WorktreeHealthy(context.Background(), wtPath) {
		t.Fatalf("expected worktree to be unhealthy after dropping its admin entry")
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("expected orphan checkout dir to still exist: %v", err)
	}

	got, err := h.m.PrepareForFix(context.Background(), tk, 1)
	if err != nil {
		t.Fatalf("PrepareForFix (orphan reconcile): %v", err)
	}
	if got != wtPath {
		t.Fatalf("reconciled path = %q, want %q", got, wtPath)
	}
	if !project.WorktreeHealthy(context.Background(), got) {
		t.Fatalf("expected reconciled worktree to be healthy")
	}
	headBranch, err := exec.Command("git", "-C", got, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD branch: %v: %s", err, headBranch)
	}
	if gotBranch := strings.TrimSpace(string(headBranch)); gotBranch != branch {
		t.Errorf("worktree branch = %q, want %q", gotBranch, branch)
	}
}

// TestPrepareForFix_ReconcilesDivergedTaskBranch is the regression test for
// issue #2347: a fix worktree whose local branch is both ahead of and behind
// its own remote head (ahead > 0 && behind > 0 — e.g. an unpushed local fix
// commit left behind by a prior run, plus a fix pushed independently from
// another clone/machine) must be deterministically reconciled with a real
// merge. The prior behavior discarded and recreated the worktree from the
// same diverged local branch, which does not fix anything — the recreated
// worktree still started ahead 2, behind 1.
func TestPrepareForFix_ReconcilesDivergedTaskBranch(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	const prNumber = 21
	const branch = "fix/diverged"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "initial fix commit")
	srcGit("checkout", "main")

	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("reconcile diverged fix worktree", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix (initial): %v", err)
	}

	// Simulate an unpushed local fix commit left behind in the worktree from a
	// prior run — local is now "ahead" of the branch's own remote head.
	if err := os.WriteFile(filepath.Join(wtPath, "local-fix.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "add", "local-fix.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", wtPath, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "-m", "unpushed local fix").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	// Independently, the branch advances on the remote (e.g. pushed from
	// another clone/machine) on a different file — local is now also "behind".
	srcGit("checkout", branch)
	if err := os.WriteFile(filepath.Join(h.src, "remote-fix.txt"), []byte("remote"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "remote fix pushed independently")
	srcGit("checkout", "main")

	got, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix (diverged reconcile): %v", err)
	}
	if got != wtPath {
		t.Fatalf("reconciled path = %q, want the same worktree %q (reconciled in place, not recreated)", got, wtPath)
	}

	if _, err := os.Stat(filepath.Join(got, "local-fix.txt")); err != nil {
		t.Errorf("reconciled worktree missing local-only commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got, "remote-fix.txt")); err != nil {
		t.Errorf("reconciled worktree missing remote-only commit: %v", err)
	}

	statusOut, statusErr := exec.Command("git", "-C", got, "status", "--porcelain").CombinedOutput()
	if statusErr != nil {
		t.Fatalf("git status: %v: %s", statusErr, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("reconciled worktree not clean: %q", statusOut)
	}

	// The branch's own remote head must now be an ancestor of local HEAD —
	// no longer "behind" — proving the reconcile actually merged it in rather
	// than just re-checking-out the same diverged local branch.
	remoteHead, err := exec.Command("git", "-C", got, "rev-parse", "origin/"+branch).Output()
	if err != nil {
		t.Fatalf("resolve origin/%s: %v", branch, err)
	}
	if out, err := exec.Command("git", "-C", got, "merge-base", "--is-ancestor", strings.TrimSpace(string(remoteHead)), "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("remote head not an ancestor of reconciled HEAD (still diverged): %v: %s", err, out)
	}
}

// TestPrepareForFix_RecreatedWorktreeReconcilesDivergedTaskBranch covers the
// recreate path from #2347: when the previous fix worktree is gone but the
// bare clone still retains the stale local branch ref, a fresh prepare must
// reconcile that local branch against origin/<branch> before dispatching.
func TestPrepareForFix_RecreatedWorktreeReconcilesDivergedTaskBranch(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	const prNumber = 23
	const branch = "fix/diverged-recreated"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "initial fix commit")
	srcGit("checkout", "main")

	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("recreate diverged fix worktree", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix (initial): %v", err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "local-fix.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "add", "local-fix.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", wtPath, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "-m", "unpushed local fix").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	srcGit("checkout", branch)
	if err := os.WriteFile(filepath.Join(h.src, "remote-fix.txt"), []byte("remote"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "remote fix pushed independently")
	srcGit("checkout", "main")

	if err := project.RemoveWorktreeReconcile(context.Background(), h.proj.ClonePath, wtPath); err != nil {
		t.Fatalf("remove old worktree: %v", err)
	}

	forkBare := filepath.Join(t.TempDir(), "fork.git")
	if out, err := exec.Command("git", "init", "--bare", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("init fork bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", h.proj.ClonePath, "remote", "add", "fork", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("add fork remote: %v: %s", err, out)
	}

	got, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix (recreated diverged reconcile): %v", err)
	}
	if got != wtPath {
		t.Fatalf("recreated path = %q, want %q", got, wtPath)
	}

	if _, err := os.Stat(filepath.Join(got, "local-fix.txt")); err != nil {
		t.Errorf("recreated worktree missing local-only commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(got, "remote-fix.txt")); err != nil {
		t.Errorf("recreated worktree missing remote-only commit: %v", err)
	}

	statusOut, statusErr := exec.Command("git", "-C", got, "status", "--porcelain").CombinedOutput()
	if statusErr != nil {
		t.Fatalf("git status: %v: %s", statusErr, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("recreated worktree not clean: %q", statusOut)
	}

	remoteHead, err := exec.Command("git", "-C", got, "rev-parse", "origin/"+branch).Output()
	if err != nil {
		t.Fatalf("resolve origin/%s: %v", branch, err)
	}
	if out, err := exec.Command("git", "-C", got, "merge-base", "--is-ancestor", strings.TrimSpace(string(remoteHead)), "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("remote head not an ancestor of recreated HEAD (still diverged): %v: %s", err, out)
	}
}

// TestPrepareForFix_PRHeadFallbackIgnoresSameNamedForkBranch covers PR-fix
// worktrees created from refs/pull/<N>/head while a configured fork remote has
// a same-named branch. Fresh preflight must compare against the fetched PR head,
// not PushRemote's fork/<branch>.
func TestPrepareForFix_PRHeadFallbackIgnoresSameNamedForkBranch(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	const prNumber = 24
	const branch = "fix/fork-name-collision"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "pr-head.txt"), []byte("pr"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "pr head")
	shaOut, err := exec.Command("git", "-C", h.src, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse pr head: %v: %s", err, shaOut)
	}
	prSHA := strings.TrimSpace(string(shaOut))
	srcGit("update-ref", "refs/pull/24/head", prSHA)
	srcGit("checkout", "main")
	srcGit("branch", "-D", branch)

	forkBare := filepath.Join(t.TempDir(), "fork.git")
	if out, err := exec.Command("git", "init", "--bare", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("init fork bare: %v: %s", err, out)
	}
	forkSrc := filepath.Join(t.TempDir(), "fork-src")
	if out, err := exec.Command("git", "clone", h.src, forkSrc).CombinedOutput(); err != nil {
		t.Fatalf("clone fork source: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", forkSrc, "checkout", "-b", branch).CombinedOutput(); err != nil {
		t.Fatalf("checkout fork branch: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(forkSrc, "fork-only.txt"), []byte("fork"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", forkSrc, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add fork: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", forkSrc, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "-m", "same named fork branch").CombinedOutput(); err != nil {
		t.Fatalf("git commit fork: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", forkSrc, "push", forkBare, branch).CombinedOutput(); err != nil {
		t.Fatalf("push fork branch: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", h.proj.ClonePath, "remote", "add", "fork", forkBare).CombinedOutput(); err != nil {
		t.Fatalf("add fork remote: %v: %s", err, out)
	}

	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("fix fork collision pr", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix: %v", err)
	}
	headSHA, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v: %s", err, headSHA)
	}
	if got := strings.TrimSpace(string(headSHA)); got != prSHA {
		t.Fatalf("worktree HEAD = %q, want PR head %q", got, prSHA)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "fork-only.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree unexpectedly merged fork branch file: %v", err)
	}
}

// TestPrepareForFix_DivergedTaskBranchConflictEscalates covers the other half
// of #2347's contract: when the diverged local and remote copies of the same
// branch genuinely conflict (both edit the same file differently), that is a
// real semantic blocker a recreate cannot paper over. PrepareForFix must
// surface an error and clean up the worktree rather than silently continuing.
func TestPrepareForFix_DivergedTaskBranchConflictEscalates(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	const prNumber = 22
	const branch = "fix/diverged-conflict"
	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, "shared.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "initial fix commit")
	srcGit("checkout", "main")

	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("diverged conflict fix worktree", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err != nil {
		t.Fatalf("PrepareForFix (initial): %v", err)
	}

	// Local edits shared.txt one way (unpushed)...
	if err := os.WriteFile(filepath.Join(wtPath, "shared.txt"), []byte("local-edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "add", "shared.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", wtPath, "-c", "user.name=Test", "-c", "user.email=test@test.com", "commit", "-m", "local edit").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	// ...while the remote edits it a conflicting way.
	srcGit("checkout", branch)
	if err := os.WriteFile(filepath.Join(h.src, "shared.txt"), []byte("remote-edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "remote edit")
	srcGit("checkout", "main")

	_, err = h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err == nil {
		t.Fatal("PrepareForFix (diverged conflict) = nil error, want a genuine-conflict error")
	}
	if !errors.Is(err, project.ErrBranchDiverged) {
		t.Errorf("PrepareForFix (diverged conflict) err = %v, want wrapping project.ErrBranchDiverged", err)
	}

	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Errorf("worktree not cleaned up after unresolved divergence conflict: stat err = %v", statErr)
	}
}

// TestPrepareForTask_AdoptRejectsMissingDir verifies adoption fails cleanly
// when the declared worktree directory does not exist.
func TestPrepareForTask_AdoptRejectsMissingDir(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	tk := task.Task{
		ID:          "deadbeef",
		Title:       "bad adopt",
		ProjectID:   h.proj.ID,
		WorktreeDir: filepath.Join(t.TempDir(), "does-not-exist"),
	}
	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); err == nil {
		t.Fatal("expected error for missing adopted worktree dir, got nil")
	}
}

// TestPrepareForTask_AdoptRejectsDefaultBranch guards the blocker: adopting a
// worktree sitting on the repo's default branch would push the agent's commits
// straight to origin's default branch with no PR. Adoption must refuse it.
func TestPrepareForTask_AdoptRejectsDefaultBranch(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	ext := filepath.Join(t.TempDir(), "orca-on-main")
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", h.proj.ClonePath, "worktree", "add", ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	tk := task.Task{ID: "cafe1234", Title: "adopt main", ProjectID: h.proj.ID, WorktreeDir: ext}
	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); err == nil {
		t.Fatal("expected error adopting a worktree on the default branch, got nil")
	}
}

// TestPrepareForTask_AdoptRejectsDetachedHead guards the empty-branch path: a
// detached-HEAD worktree has no branch to push, so adoption must refuse rather
// than silently proceed with an empty branch.
func TestPrepareForTask_AdoptRejectsDetachedHead(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	ext := filepath.Join(t.TempDir(), "orca-detached")
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", h.proj.ClonePath, "worktree", "add", "--detach", ext, "main").CombinedOutput(); err != nil {
		t.Fatalf("git worktree add --detach: %v: %s", err, out)
	}

	tk := task.Task{ID: "beef5678", Title: "adopt detached", ProjectID: h.proj.ID, WorktreeDir: ext}
	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); err == nil {
		t.Fatal("expected error adopting a detached-HEAD worktree, got nil")
	}
}
