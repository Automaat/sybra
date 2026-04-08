//go:build !short

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
	"github.com/Automaat/synapse/internal/workflow"
	"github.com/Automaat/synapse/internal/worktree"
)

var (
	testBinDir    string
	testBuildOnce sync.Once
)

func buildTestBinaries(t *testing.T) string {
	t.Helper()
	testBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "synapse-test-bins-*")
		if err != nil {
			panic(err)
		}
		// Build fake claude.
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "claude"), "./cmd/fake-claude")
		if out, err := cmd.CombinedOutput(); err != nil {
			panic("build fake-claude: " + err.Error() + "\n" + string(out))
		}
		// Build real synapse-cli.
		cmd = exec.Command("go", "build", "-o", filepath.Join(dir, "synapse-cli"), "./cmd/synapse-cli")
		if out, err := cmd.CombinedOutput(); err != nil {
			panic("build synapse-cli: " + err.Error() + "\n" + string(out))
		}
		testBinDir = dir
	})
	return testBinDir
}

func e2eLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// setupE2E wires up real task store, workflow engine, agent manager with fake claude.
type e2eEnv struct {
	tasks   *task.Manager
	agents  *agent.Manager
	engine  *workflow.Engine
	taskDir string
	cancel  context.CancelFunc
}

func setupE2E(t *testing.T, scenario string) *e2eEnv {
	t.Helper()

	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", scenario)

	// Use os.MkdirTemp instead of t.TempDir to avoid cleanup races with
	// background goroutines (agent processes, synapse-cli writes).
	taskDir, err := os.MkdirTemp("", "synapse-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(taskDir) })
	t.Setenv("SYNAPSE_HOME", taskDir)

	// Create tasks subdir (synapse-cli expects SYNAPSE_HOME/tasks/).
	tasksDir := filepath.Join(taskDir, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	logger := e2eLogger()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	tm := tmux.NewManager()
	agentMgr := agent.NewManager(ctx, tm, func(string, any) {}, logger, t.TempDir())

	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	// Copy test workflow.
	src, err := os.ReadFile("internal/workflow/testdata/test-simple.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "test-simple.yaml"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        taskMgr,
		Logger:       logger,
		AgentChecker: agentMgr.HasRunningAgentForTask,
	})
	agentOrch := newAgentOrchestrator(taskMgr, nil, agentMgr, nil, logger, wm, nil)

	ta := &taskAdapter{tasks: taskMgr}
	aa := &agentAdapter{agents: agentMgr, agentOrch: agentOrch, tasks: taskMgr}
	engine := workflow.NewEngine(wfStore, ta, aa, logger)

	agentMgr.SetOnComplete(func(ag *agent.Agent) {
		var result string
		for _, ev := range ag.Output() {
			if ev.Type == "result" {
				result = ev.Content
			}
		}
		engine.HandleAgentComplete(ag.TaskID, ag.ID, result)
	})

	return &e2eEnv{
		tasks:   taskMgr,
		agents:  agentMgr,
		engine:  engine,
		taskDir: taskDir,
		cancel:  cancel,
	}
}

// waitFor polls a condition with timeout.
func waitFor(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for: %s", desc)
		case <-time.After(50 * time.Millisecond):
			if fn() {
				return
			}
		}
	}
}

func TestE2E_HeadlessAgent_Success(t *testing.T) {
	env := setupE2E(t, "success")

	created, err := env.tasks.Create("test task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Wait for triage agent to complete and engine to advance.
	waitFor(t, 10*time.Second, "workflow advances past triage", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.CurrentStep != "triage"
	})

	// Verify the triage agent ran and produced output.
	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow == nil {
		t.Fatal("expected workflow to be set")
	}
}

func TestE2E_HeadlessAgent_ArgsVerification(t *testing.T) {
	argsLog := filepath.Join(t.TempDir(), "args.log")
	env := setupE2E(t, "success")
	t.Setenv("FAKE_CLAUDE_ARGS_LOG", argsLog)

	created, err := env.tasks.Create("test task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Wait for agent to complete.
	waitFor(t, 10*time.Second, "args log written", func() bool {
		_, err := os.Stat(argsLog)
		return err == nil
	})

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)

	// Verify key flags.
	if !strings.Contains(args, "--output-format\nstream-json") {
		t.Errorf("expected --output-format stream-json in args:\n%s", args)
	}
	if !strings.Contains(args, "-p") {
		t.Errorf("expected -p flag in args:\n%s", args)
	}
	if !strings.Contains(args, "--model\nsonnet") {
		t.Errorf("expected --model sonnet in args:\n%s", args)
	}
}

func TestE2E_HeadlessAgent_FailExit(t *testing.T) {
	env := setupE2E(t, "fail_exit")

	created, err := env.tasks.Create("test task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Wait for the workflow to attempt retries (max_retries: 3 on triage).
	// Each retry spawns fake claude which exits 1, so eventually retries exhaust.
	waitFor(t, 30*time.Second, "workflow moves past triage retries", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		if tk.Workflow == nil {
			return false
		}
		// Either advanced past triage or workflow state changed.
		return tk.Workflow.CurrentStep != "triage" || tk.Workflow.State == workflow.ExecFailed || tk.Workflow.State == workflow.ExecCompleted
	})
}

func TestE2E_WorkflowWithSynapseCLI(t *testing.T) {
	env := setupE2E(t, "triage")

	created, err := env.tasks.Create("implement auth", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// The "triage" scenario runs: synapse-cli update <id> --status todo --tags small
	// Then engine re-reads task and sees status=todo → advances to set_in_progress → implement.
	// The implement step spawns another fake claude (success scenario), which also completes,
	// advancing to evaluate. We just need to verify the engine advanced past triage.
	waitFor(t, 15*time.Second, "workflow advances past triage", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		if tk.Workflow == nil {
			return false
		}
		// Engine should have advanced past triage to any later step.
		switch tk.Workflow.CurrentStep {
		case "triage":
			return false
		default:
			return true
		}
	})

	tk, _ := env.tasks.Get(created.ID)
	// Task should have progressed — either in-progress (at implement) or
	// further along. The exact status depends on timing.
	if tk.Workflow.CurrentStep == "triage" {
		t.Fatal("expected workflow to advance past triage")
	}
}
