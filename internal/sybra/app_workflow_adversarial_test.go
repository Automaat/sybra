package sybra

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

func testAdversarialCheckCommandsIgnoreWorktreeRepoConfig(t *testing.T, which, want string, getCommands func(*checkConfigGetterAdapter, context.Context, string) []string) {
	t.Helper()
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
	repoCfg := "checks:\n" +
		"  codegen:\n" +
		"    - echo TRUSTED_CODEGEN\n" +
		"  verify:\n" +
		"    - echo TRUSTED_VERIFY\n"
	if err := os.WriteFile(filepath.Join(src, ".sybra.yaml"), []byte(repoCfg), 0o644); err != nil {
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
	// trusted default-branch .sybra.yaml wins over both worktree edits and the
	// app-level fallback.
	projYAML := "id: owner/repo\nname: repo\nowner: owner\nrepo: repo\nurl: " + bare +
		"\nclone_path: " + bare + "\ntype: pet\nchecks:\n  codegen:\n    - echo APP_CODEGEN\n  verify:\n    - echo APP_VERIFY\n"
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
	if err := os.WriteFile(filepath.Join(wtPath, ".sybra.yaml"), []byte("checks:\n  codegen:\n    - echo UNTRUSTED_CODEGEN\n  verify:\n    - echo UNTRUSTED_VERIFY\nsetup:\n  - echo UNTRUSTED_SETUP\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	adapter := &checkConfigGetterAdapter{
		tasks:    taskMgr,
		projects: projects,
		mgr:      wm,
	}

	got := getCommands(adapter, t.Context(), tk.ID)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("%s = %#v, want only trusted default-branch command %q", which, got, want)
	}

	gotSetup := adapter.SetupCommands(t.Context(), tk.ID)
	for _, c := range gotSetup {
		if c == "echo UNTRUSTED_SETUP" {
			t.Fatalf("SetupCommands leaked the untrusted worktree setup: %#v", gotSetup)
		}
	}
}

// TestAdversarialVerifyCommandsIgnoreWorktreeRepoConfig is the regression for
// issue #1519's checks.verify half: verify_checks executes
// checkConfigGetterAdapter.VerifyCommands' result unsandboxed via `sh -c`
// (engine_steps_verify_checks.go), so those commands must never be sourced
// from the checked-out worktree's own .sybra.yaml.
func TestAdversarialVerifyCommandsIgnoreWorktreeRepoConfig(t *testing.T) {
	testAdversarialCheckCommandsIgnoreWorktreeRepoConfig(
		t,
		"VerifyCommands",
		"echo TRUSTED_VERIFY",
		func(a *checkConfigGetterAdapter, ctx context.Context, taskID string) []string {
			return a.VerifyCommands(ctx, taskID)
		},
	)
}

// TestAdversarialCodegenCommandsIgnoreWorktreeRepoConfig is the checks.codegen
// half of issue #1519: codegen_gate executes these commands unsandboxed via
// `sh -c`, so the checked-out worktree's own .sybra.yaml must never be able to
// replace the trusted default-branch config.
func TestAdversarialCodegenCommandsIgnoreWorktreeRepoConfig(t *testing.T) {
	testAdversarialCheckCommandsIgnoreWorktreeRepoConfig(
		t,
		"CodegenCommands",
		"echo TRUSTED_CODEGEN",
		func(a *checkConfigGetterAdapter, ctx context.Context, taskID string) []string {
			return a.CodegenCommands(ctx, taskID)
		},
	)
}
