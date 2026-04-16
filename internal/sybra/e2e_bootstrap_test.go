//go:build !short

package sybra

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// bootstrapE2E is a full-stack harness wired like setupE2E but with a real
// project.Store, a real bare git repo (with .sybra.yaml committed upstream),
// and the worktree manager configured with LogsDir + a real Projects store.
// This exercises the bootstrap chain end-to-end: AgentOrchestrator.StartAgent
// → autoAssignProject → worktrees.PrepareForTask → resolveSetupCommands
// (repo .sybra.yaml + app SetupCommands merged) → runSetup → fake-claude.
type bootstrapE2E struct {
	tasks        *task.Manager
	agents       *agent.Manager
	projects     *project.Store
	agentOrch    *AgentOrchestrator
	worktreesDir string
	logsDir      string
	projectID    string
	cancel       context.CancelFunc
}

func setupBootstrapE2E(t *testing.T, repoSetup, appSetup []string) *bootstrapE2E {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "success")

	home, err := os.MkdirTemp("", "sybra-bootstrap-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("SYBRA_HOME", home)

	tasksDir := filepath.Join(home, "tasks")
	projDir := filepath.Join(home, "projects")
	clonesDir := filepath.Join(home, "clones")
	wtDir := filepath.Join(home, "worktrees")
	logsDir := filepath.Join(home, "logs")
	for _, d := range []string{tasksDir, projDir, clonesDir, wtDir, logsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Upstream src + bare clone. Committing .sybra.yaml into src here means
	// FetchOrigin on the bare pulls it into refs/remotes/origin/main, which
	// is the ref PrepareForTask branches from.
	src := filepath.Join(home, "upstream-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", src, "init", "-b", "main"},
		{"git", "-C", src, "config", "user.email", "e2e@example.com"},
		{"git", "-C", src, "config", "user.name", "E2E"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(repoSetup) > 0 {
		var sb strings.Builder
		sb.WriteString("setup:\n")
		for _, c := range repoSetup {
			sb.WriteString("  - " + c + "\n")
		}
		if err := os.WriteFile(filepath.Join(src, ".sybra.yaml"), []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"git", "-C", src, "add", "-A"},
		{"git", "-C", src, "commit", "-m", "e2e seed"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}

	bare := filepath.Join(clonesDir, "e2e", "proj.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "clone", "--bare", src, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git fetch: %v: %s", err, out)
	}

	// Project store + registered project pointing at the bare we just built.
	// Writing YAML directly bypasses Store.Create (which would try to clone
	// from a URL that isn't reachable in tests).
	projStore, err := project.NewStore(projDir, clonesDir)
	if err != nil {
		t.Fatal(err)
	}
	var projYAML strings.Builder
	projYAML.WriteString("id: e2e/proj\nname: proj\nowner: e2e\nrepo: proj\nurl: ")
	projYAML.WriteString(bare)
	projYAML.WriteString("\nclone_path: ")
	projYAML.WriteString(bare)
	projYAML.WriteString("\ntype: pet\nstatus: ready\n")
	if len(appSetup) > 0 {
		projYAML.WriteString("setup_commands:\n")
		for _, c := range appSetup {
			projYAML.WriteString("  - " + c + "\n")
		}
	}
	projYAML.WriteString("created_at: 2026-04-16T00:00:00Z\nupdated_at: 2026-04-16T00:00:00Z\n")
	if err := os.WriteFile(filepath.Join(projDir, "e2e--proj.yaml"), []byte(projYAML.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// Task store + manager.
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)

	// Agent manager + fake claude.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	agentLogDir := filepath.Join(logsDir, "agent-manager")
	if err := os.MkdirAll(agentLogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logger := e2eLogger()
	agentMgr := agent.NewManager(ctx, func(string, any) {}, logger, agentLogDir)
	agentMgr.SetDefaultProvider("claude")

	wm := worktree.New(worktree.Config{
		WorktreesDir: wtDir,
		Projects:     projStore,
		Tasks:        taskMgr,
		Logger:       logger,
		LogsDir:      logsDir,
		AgentChecker: agentMgr.HasRunningAgentForTask,
	})
	agentOrch := newAgentOrchestrator(taskMgr, projStore, agentMgr, nil, logger, wm, nil)

	return &bootstrapE2E{
		tasks:        taskMgr,
		agents:       agentMgr,
		projects:     projStore,
		agentOrch:    agentOrch,
		worktreesDir: wtDir,
		logsDir:      logsDir,
		projectID:    "e2e/proj",
		cancel:       cancel,
	}
}

// TestE2E_BootstrapRunsBeforeAgent_MergedRepoAndApp is the top-level
// regression guard for the aa9ba123 class of failure. It wires the full
// stack (projects + bare + .sybra.yaml + worktree manager + agent
// orchestrator + fake-claude) and confirms that a single StartAgent call
// runs the merged bootstrap (repo first, then app) inside the freshly
// created worktree before the agent process spawns.
func TestE2E_BootstrapRunsBeforeAgent_MergedRepoAndApp(t *testing.T) {
	env := setupBootstrapE2E(t,
		[]string{"echo repo > repo.marker", "echo repo2 > repo2.marker"},
		[]string{"echo app > app.marker"},
	)

	tk, err := env.tasks.Create("bootstrap e2e task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(env.projectID)}); err != nil {
		t.Fatal(err)
	}

	ag, err := env.agentOrch.StartAgent(tk.ID, "headless", "any prompt", false)
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	// Wait for the fake-claude process to complete so we know the full
	// StartAgent → PrepareForTask → runSetup → agent.Run path executed.
	// The setup markers must exist the moment the agent is running
	// (setup runs before the agent spawns).
	deadline := time.After(10 * time.Second)
	for {
		state := ag.GetState()
		if state == agent.StateStopped {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for agent completion; state=%s", state)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Locate the worktree directory the orchestrator picked. The worktree
	// naming convention is <slug>-<id8> (see task.DirName), stored under
	// env.worktreesDir.
	entries, err := os.ReadDir(env.worktreesDir)
	if err != nil {
		t.Fatalf("read worktrees dir: %v", err)
	}
	var wtPath string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), tk.ID[:8]) {
			wtPath = filepath.Join(env.worktreesDir, e.Name())
			break
		}
	}
	if wtPath == "" {
		t.Fatalf("no worktree created for task %s under %s (entries=%v)", tk.ID, env.worktreesDir, entries)
	}

	for _, m := range []string{"repo.marker", "repo2.marker", "app.marker"} {
		p := filepath.Join(wtPath, m)
		if _, statErr := os.Stat(p); statErr != nil {
			t.Errorf("marker %q missing — merged bootstrap did not run: %v", m, statErr)
		}
	}

	// Setup log must have been written to ~/.sybra/logs/worktrees/<task>-setup.log
	logPath := filepath.Join(env.logsDir, "worktrees", tk.ID+"-setup.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read setup log at %s: %v", logPath, err)
	}
	// Repo commands must have been logged before the app command.
	if bytes.Index(data, []byte("repo.marker")) > bytes.Index(data, []byte("app.marker")) {
		t.Errorf("setup order wrong — app command ran before repo. log:\n%s", data)
	}
}

// TestE2E_BootstrapFailure_AbortsAgentStart is the negative case: when any
// setup command exits non-zero, StartAgent must fail before the agent
// process spawns. Without this, the server would waste tokens running
// agents on worktrees missing required toolchain.
func TestE2E_BootstrapFailure_AbortsAgentStart(t *testing.T) {
	env := setupBootstrapE2E(t, []string{"exit 13"}, nil)

	tk, err := env.tasks.Create("failing bootstrap", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(env.projectID)}); err != nil {
		t.Fatal(err)
	}

	ag, err := env.agentOrch.StartAgent(tk.ID, "headless", "any prompt", false)
	if err == nil {
		t.Fatalf("expected StartAgent to fail, got agent %s", ag.ID)
	}
	if !strings.Contains(err.Error(), "exit 13") {
		t.Errorf("error does not reference failing command: %v", err)
	}
}
