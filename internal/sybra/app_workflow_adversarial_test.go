package sybra

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// TestAdversarialVerifyCommandsIgnoreWorktreeRepoConfig is the regression for
// issue #1519's checks.verify half: verify_checks executes
// checkConfigGetterAdapter.VerifyCommands' result unsandboxed via `sh -c`
// (engine_steps_verify_checks.go), so those commands must never be sourced
// from the checked-out worktree's own .sybra.yaml — a compromised or
// prompt-injected agent could plant a malicious checks.verify block there and
// get it executed on the next verify pass. Only the project's trusted
// default-branch .sybra.yaml (or the app-level project config) may supply
// verify commands.
func TestAdversarialVerifyCommandsIgnoreWorktreeRepoConfig(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")

	srcGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", src}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	for _, args := range [][]string{
		{"init", "-b", "main", src},
		{"-C", src, "config", "user.email", "test@test.com"},
		{"-C", src, "config", "user.name", "Test"},
		{"-C", src, "config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srcGit("add", ".")
	srcGit("commit", "-m", "init")

	bare := filepath.Join(tmp, "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", src, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git fetch: %v: %s", err, out)
	}

	projects, err := project.NewStore(filepath.Join(tmp, "projects"), filepath.Join(tmp, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	// Project-level trusted config: not the checked-out worktree, and the
	// default-branch .sybra.yaml has no checks.verify of its own, so this is
	// the command MergeChecks must fall back to.
	projYAML := "id: owner/repo\nname: repo\nowner: owner\nrepo: repo\nurl: " + bare +
		"\nclone_path: " + bare + "\ntype: pet\nchecks:\n  verify:\n    - echo TRUSTED_DEFAULT_BRANCH\n"
	if err := os.WriteFile(filepath.Join(tmp, "projects", "owner--repo.yaml"), []byte(projYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	taskStore, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)
	tk, err := taskMgr.Create("adversarial verify commands", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	projectID := "owner/repo"
	tk, err = taskMgr.Update(tk.ID, task.Update{ProjectID: &projectID})
	if err != nil {
		t.Fatal(err)
	}

	logger := discardLogger()
	wm := worktree.New(worktree.Config{
		WorktreesDir: filepath.Join(tmp, "worktrees"),
		Projects:     projects,
		Tasks:        taskMgr,
		Logger:       logger,
	})

	wtPath, err := wm.PrepareForTask(t.Context(), tk, nil)
	if err != nil {
		t.Fatalf("PrepareForTask: %v", err)
	}

	// Attacker-controlled .sybra.yaml planted directly in the task's own
	// worktree (e.g. by a compromised implementation agent).
	if err := os.WriteFile(filepath.Join(wtPath, ".sybra.yaml"), []byte("checks:\n  verify:\n    - echo UNTRUSTED_WORKTREE\nsetup:\n  - echo UNTRUSTED_SETUP\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	getter := &checkConfigGetterAdapter{
		tasks:    taskMgr,
		projects: projects,
		mgr:      wm,
	}

	got := getter.VerifyCommands(t.Context(), tk.ID)
	if len(got) != 1 || got[0] != "echo TRUSTED_DEFAULT_BRANCH" {
		t.Fatalf("VerifyCommands = %#v, want only trusted default-branch command", got)
	}

	gotSetup := getter.SetupCommands(t.Context(), tk.ID)
	for _, c := range gotSetup {
		if c == "echo UNTRUSTED_SETUP" {
			t.Fatalf("SetupCommands leaked the untrusted worktree setup: %#v", gotSetup)
		}
	}
}
