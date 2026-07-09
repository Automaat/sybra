package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestPrepareForFix_UntrustedRepoSetupNotExecuted is the regression for issue
// #1519: PrepareForFix checks out a PR's head branch (possibly a fork, or a
// Renovate branch), which is attacker-controlled content. A .sybra.yaml
// `setup:` command declared there must never run — only the project's
// default-branch .sybra.yaml (trusted, since Sybra itself controls what
// lands there via reviewed PRs) is a valid source of setup commands.
func TestPrepareForFix_UntrustedRepoSetupNotExecuted(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// Trusted config on main: this must run.
	if err := os.WriteFile(filepath.Join(h.src, ".sybra.yaml"), []byte("setup:\n  - touch trusted-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "trusted setup")

	const prNumber = 42
	const branch = "renovate/bump-dep"
	srcGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(h.src, ".sybra.yaml"), []byte("setup:\n  - touch evil-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "malicious setup override")
	srcGit("checkout", "main")

	h.m.prBranch = func(_ string, _ int) (string, error) { return branch, nil }

	tk, err := h.tasks.Create("fix renovate pr", "", task.AgentModeHeadless)
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

	if _, err := os.Stat(filepath.Join(wtPath, "evil-marker")); err == nil {
		t.Fatal("malicious setup command from the untrusted PR branch's .sybra.yaml was executed")
	}
	if _, err := os.Stat(filepath.Join(wtPath, "trusted-marker")); err != nil {
		t.Errorf("trusted default-branch setup command did not run: %v", err)
	}
}

// TestPrepareForReview_UntrustedRepoSetupNotExecuted mirrors
// TestPrepareForFix_UntrustedRepoSetupNotExecuted for the read-only review
// worktree, whose checked-out ref is a fork PR's head via
// refs/pull/<N>/head — never reachable as an origin branch.
func TestPrepareForReview_UntrustedRepoSetupNotExecuted(t *testing.T) {
	h := prepareHarness(t, nil, 0)

	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", h.src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// Trusted config on main: this must run.
	if err := os.WriteFile(filepath.Join(h.src, ".sybra.yaml"), []byte("setup:\n  - touch trusted-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "trusted setup")

	const prNumber = 55
	const forkBranch = "fork/pr-branch"
	srcGit("checkout", "-b", forkBranch)
	if err := os.WriteFile(filepath.Join(h.src, ".sybra.yaml"), []byte("setup:\n  - touch evil-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "malicious setup override")
	shaOut, err := exec.Command("git", "-C", h.src, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, shaOut)
	}
	prSHA := strings.TrimSpace(string(shaOut))
	// Only reachable via refs/pull/<N>/head, mirroring a fork PR — never
	// lands under refs/remotes/origin/*.
	srcGit("update-ref", "refs/pull/55/head", prSHA)
	srcGit("checkout", "main")
	srcGit("branch", "-D", forkBranch)

	h.m.prBranch = func(_ string, _ int) (string, error) { return forkBranch, nil }

	tk, err := h.tasks.Create("review fork pr", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": h.proj.ID,
		"pr_number":  prNumber,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	wtPath, err := h.m.PrepareForReview(context.Background(), tk)
	if err != nil {
		t.Fatalf("PrepareForReview: %v", err)
	}

	if _, err := os.Stat(filepath.Join(wtPath, "evil-marker")); err == nil {
		t.Fatal("malicious setup command from the untrusted PR head's .sybra.yaml was executed")
	}
	if _, err := os.Stat(filepath.Join(wtPath, "trusted-marker")); err != nil {
		t.Errorf("trusted default-branch setup command did not run: %v", err)
	}
}
