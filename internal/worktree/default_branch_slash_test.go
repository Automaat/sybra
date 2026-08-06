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

// TestAdoptRefusesSlashedDefaultBranch is the acceptance test for #3129. The
// guard that stops an agent pushing straight to the default branch with no PR
// compares CurrentBranch against DefaultBranch, and DefaultBranch used to
// truncate at the last slash — so on a project whose default is release/2.0 it
// returned "2.0", the comparison never matched, and the guard passed a worktree
// sitting on the branch it exists to refuse.
func TestAdoptRefusesSlashedDefaultBranch(t *testing.T) {
	for _, defaultBranch := range []string{"release/2.0", "main"} {
		t.Run(defaultBranch, func(t *testing.T) {
			h := slashDefaultHarness(t, defaultBranch)

			onDefault := filepath.Join(t.TempDir(), "adopted-on-default")
			mustGit(t, h.proj.ClonePath, "worktree", "add", onDefault, defaultBranch)
			tk := task.Task{ID: "slash001", Title: "adopt default", ProjectID: h.proj.ID, WorktreeDir: onDefault}
			_, err := h.m.PrepareForTask(context.Background(), tk, nil)
			if err == nil {
				t.Fatal("adopted a worktree sitting on the default branch")
			}
			// The full phrase, not just the branch name: the name alone also
			// appears in the temp-dir path every adoption error carries, so a
			// substring check passes on any failure at all.
			want := `checked out on default branch "` + defaultBranch + `"`
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q is not the default-branch refusal for %q", err, defaultBranch)
			}

			// Control: the refusal must come from the branch being the default
			// one, not from every adoption failing.
			onFeature := filepath.Join(t.TempDir(), "adopted-on-feature")
			mustGit(t, h.proj.ClonePath, "worktree", "add", "-b", "feat/work", onFeature, defaultBranch)
			feat := task.Task{ID: "slash002", Title: "adopt feature", ProjectID: h.proj.ID, WorktreeDir: onFeature}
			if _, err := h.m.PrepareForTask(context.Background(), feat, nil); err != nil {
				t.Fatalf("adopting a feature-branch worktree: %v", err)
			}
		})
	}
}

// TestPrepareForTask_SlashedDefaultBranchBaseRef pins the other half: the base
// ref new worktrees branch from is built by concatenating the default branch
// name, so a truncated name names a ref that does not exist and worktree
// creation fails outright.
func TestPrepareForTask_SlashedDefaultBranchBaseRef(t *testing.T) {
	h := slashDefaultHarness(t, "release/2.0")

	tk, err := h.tasks.Store().Create("slashed base", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask on a slashed default branch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Errorf("worktree was not branched off the default branch's content: %v", err)
	}
}

// slashDefaultHarness builds a preparedHarness whose project default branch is
// defaultBranch, which prepareHarness hardcodes to main.
func slashDefaultHarness(t *testing.T, defaultBranch string) preparedHarness {
	t.Helper()
	h := prepareHarness(t, nil, 0)

	mustGit(t, h.src, "branch", "-m", "main", defaultBranch)
	mustGit(t, h.proj.ClonePath, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	mustGit(t, h.proj.ClonePath, "fetch", "origin", "+refs/heads/*:refs/heads/*")
	mustGit(t, h.proj.ClonePath, "symbolic-ref", "HEAD", "refs/heads/"+defaultBranch)
	if defaultBranch != "main" {
		mustGit(t, h.proj.ClonePath, "branch", "-D", "main")
		mustGit(t, h.proj.ClonePath, "update-ref", "-d", "refs/remotes/origin/main")
	}

	// Asserted through git, not through project.DefaultBranch: a regression
	// there must fail at the guard it breaks, not at this setup check.
	out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", h.proj.ClonePath, "symbolic-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("git symbolic-ref: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "refs/heads/"+defaultBranch {
		t.Fatalf("harness HEAD = %q, want refs/heads/%s", got, defaultBranch)
	}
	return h
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "safe.bareRepository=all", "-C", dir}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
