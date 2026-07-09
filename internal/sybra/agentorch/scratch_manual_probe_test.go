package agentorch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// TestManualProbe_NestedFixDispatchActuallyStarts simulates production wiring:
// the conflictRecovery callback synchronously re-enters StartAgentWithAssignment
// for the SAME taskID (like RecoverStaleBranchConflict -> handlePRIssue ->
// workflow "fix" step dispatch does), and asserts a real agent actually starts
// instead of bailing with ErrDispatchInFlight.
func TestManualProbe_NestedFixDispatchActuallyStarts(t *testing.T) {
	bare, src := initConflictingRepo(t)

	projDir := filepath.Join(t.TempDir(), "projects")
	os.MkdirAll(projDir, 0o755)
	clonesDir := filepath.Join(t.TempDir(), "clones")
	projStore, err := project.NewStore(projDir, clonesDir)
	if err != nil {
		t.Fatal(err)
	}
	proj := project.Project{
		ID: "test/proj", Name: "proj", Owner: "test", Repo: "proj",
		URL: bare, ClonePath: bare, Type: project.ProjectTypePet,
		Status: project.ProjectStatusReady, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	writeTestProject(t, projDir, proj)

	taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(taskStore, nil)

	wtMgr := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(), Projects: projStore, Tasks: tasks,
		Logger: discardSlogLogger(), LogsDir: t.TempDir(), SetupTimeout: 30 * time.Second,
	})

	tk, err := tasks.Create("conflicting task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, _ = tasks.Get(tk.ID)

	wtPath, err := wtMgr.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatalf("initial PrepareForTask: %v", err)
	}
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(wtPath, "config", "user.email", "test@test.com")
	run(wtPath, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("branch edit\n"), 0o644)
	run(wtPath, "add", "README.md")
	run(wtPath, "commit", "-m", "branch edit")

	os.WriteFile(filepath.Join(src, "README.md"), []byte("upstream edit\n"), 0o644)
	run(src, "add", "README.md")
	run(src, "commit", "-m", "upstream edit")

	// Fake claude CLI on PATH so a nested dispatch can actually "start" without
	// spending real model credits.
	fakebin := t.TempDir()
	fakeClaude := filepath.Join(fakebin, "claude")
	os.WriteFile(fakeClaude, []byte("#!/usr/bin/env bash\n"+
		"printf '{\"type\":\"system\",\"session_id\":\"fake-session\"}\\n'\n"+
		"printf '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"fake-session\",\"result\":\"done\",\"total_cost_usd\":0.01,\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}\\n'\n"),
		0o755)
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))

	agentMgr, err := agent.NewManager(context.Background(), func(string, any) {}, discardSlogLogger(), t.TempDir(), agent.ManagerConfig{
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	researchDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Agent.ResearchMachineDir = researchDir
	o := New(tasks, projStore, agentMgr, nil, discardSlogLogger(), wtMgr, cfg)

	var nestedErr error
	var nestedAgentStarted bool
	o.SetConflictRecovery(func(taskID string) bool {
		// Flip the task to TaskTypeResearch so the nested dispatch's own
		// resolveDispatchDir call skips worktree prep (skipWT=true) instead of
		// re-hitting the same unresolved conflict — isolating exactly what the
		// fix under test guards: whether the nested same-task dispatch call
		// observes the outer claim as free. In production the real "fix" step
		// prepares via PrepareForFix (no rebase), which is the analogous
		// conflict-free path for a same-task nested dispatch.
		if _, err := tasks.Update(taskID, task.Update{TaskType: task.Ptr(task.TaskTypeResearch)}); err != nil {
			t.Fatalf("flip task to research type: %v", err)
		}
		// Re-enter the SAME choke point for the SAME task — this mirrors the
		// production "fix" step's own StartAgentWithAssignment call nested
		// inside the still-executing outer call.
		ag, _, err := o.StartAgentWithAssignment(taskID, "headless", "fix it", false, true, "", workflow.AgentAssignment{})
		nestedErr = err
		nestedAgentStarted = ag != nil
		return err == nil
	})

	_, _, err = o.StartAgentWithAssignment(tk.ID, "headless", "prompt", false, false, "", workflow.AgentAssignment{})
	t.Logf("outer StartAgentWithAssignment err = %v", err)
	t.Logf("nested (fix-step) dispatch err = %v, agentStarted = %v", nestedErr, nestedAgentStarted)

	if nestedErr != nil {
		t.Fatalf("nested fix-step dispatch failed: %v (this is the bug: ErrDispatchInFlight means claim was still held)", nestedErr)
	}
	if !nestedAgentStarted {
		t.Fatal("nested fix-step dispatch did not start an agent")
	}
}
