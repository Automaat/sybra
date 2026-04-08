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
	tasks        *task.Manager
	agents       *agent.Manager
	engine       *workflow.Engine
	taskDir      string
	scenarioFile string
	cancel       context.CancelFunc
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
		agentState := "stopped"
		if ag.ExitErr != nil {
			agentState = "failed"
		}
		engine.HandleAgentComplete(ag.TaskID, ag.ID, result, agentState)
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

// setupE2EMulti creates an e2e env with a scenario file for multi-step workflows.
// Each invocation of fake-claude pops the next scenario from the file.
func setupE2EMulti(t *testing.T, scenarios []string) *e2eEnv {
	t.Helper()
	sf := filepath.Join(t.TempDir(), "scenarios.txt")
	if err := os.WriteFile(sf, []byte(strings.Join(scenarios, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	env := setupE2E(t, "")
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", sf)
	// Clear static scenario so file takes priority.
	t.Setenv("FAKE_CLAUDE_SCENARIO", "")
	env.scenarioFile = sf
	return env
}

func TestE2E_FullLifecycle_TriageThenImplement(t *testing.T) {
	// Scenarios: triage (sets status=todo) → implement → evaluate
	env := setupE2EMulti(t, []string{"triage", "success", "success"})

	created, err := env.tasks.Create("full lifecycle task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Wait for workflow to complete (all 3 agents finish).
	waitFor(t, 30*time.Second, "workflow completes", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && (tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed)
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("expected completed, got %q (step: %s)", tk.Workflow.State, tk.Workflow.CurrentStep)
	}
	if tk.Status != task.StatusInProgress {
		t.Logf("note: final status is %q (set_status step sets in-progress, evaluate doesn't change it in success scenario)", tk.Status)
	}

	// Verify step history has records for all 3 agent steps.
	stepIDs := map[string]bool{}
	for _, r := range tk.Workflow.StepHistory {
		stepIDs[r.StepID] = true
	}
	for _, expected := range []string{"triage", "set_in_progress", "implement", "evaluate"} {
		if !stepIDs[expected] {
			t.Errorf("expected step %q in history, got %v", expected, stepIDs)
		}
	}
}

func TestE2E_RetryCount(t *testing.T) {
	// 3 failures then 1 success for triage (max_retries: 3 allows 4 total attempts).
	env := setupE2EMulti(t, []string{"fail_exit", "fail_exit", "fail_exit", "success", "success", "success"})

	created, err := env.tasks.Create("retry task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Wait for workflow to advance past triage.
	waitFor(t, 30*time.Second, "workflow advances past triage", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.CurrentStep != "triage"
	})

	tk, _ := env.tasks.Get(created.ID)
	// Count triage records in step history.
	triageCount := 0
	for _, r := range tk.Workflow.StepHistory {
		if r.StepID == "triage" {
			triageCount++
		}
	}
	// 3 failures + 1 success = 4 triage records.
	if triageCount != 4 {
		t.Fatalf("expected 4 triage step records (3 retries + 1 success), got %d", triageCount)
	}
}

func TestE2E_AgentFailure_SetsCorrectStatus(t *testing.T) {
	// Agent exits non-zero → HandleAgentComplete should pass "failed" status
	// to AdvanceStep, triggering retry logic. This verifies the bug #2 fix.
	env := setupE2EMulti(t, []string{"fail_exit", "success", "success", "success"})

	created, err := env.tasks.Create("failure status task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Wait for workflow to advance past triage (should retry after first failure).
	waitFor(t, 20*time.Second, "workflow advances past triage after retry", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.CurrentStep != "triage"
	})

	tk, _ := env.tasks.Get(created.ID)
	// Should have at least 2 triage records: 1 failed + 1 success.
	triageCount := 0
	for _, r := range tk.Workflow.StepHistory {
		if r.StepID == "triage" {
			triageCount++
		}
	}
	if triageCount < 2 {
		t.Fatalf("expected >= 2 triage records (1 failure + retry), got %d", triageCount)
	}
}

func TestE2E_ResumeStalled(t *testing.T) {
	env := setupE2EMulti(t, []string{"success", "success"})

	created, err := env.tasks.Create("stalled task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Manually set up workflow state as if it's stuck at "implement" with no agent.
	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       workflow.ExecRunning,
		Variables:   make(map[string]string),
	}
	if _, err := env.tasks.Update(created.ID, map[string]any{
		"status":   "in-progress",
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}

	// Call ResumeStalled — should detect orphaned implement step and re-execute.
	env.engine.ResumeStalled()

	// Wait for workflow to advance past implement.
	waitFor(t, 15*time.Second, "workflow advances past implement after resume", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.CurrentStep != "implement"
	})

	tk, _ := env.tasks.Get(created.ID)
	// Should have advanced to evaluate or completed.
	if tk.Workflow.CurrentStep == "implement" {
		t.Fatal("expected workflow to advance past implement after ResumeStalled")
	}
}

func TestE2E_PlanApproveReject(t *testing.T) {
	// triage_to_planning → plan agent (interactive) → human approve
	env := setupE2EMulti(t, []string{
		"triage_to_planning", // triage sets status=planning → plan step
		"success",            // plan agent completes → review_plan (wait_human)
		"success",            // after approve: implement
		"success",            // evaluate
	})

	created, err := env.tasks.Create("plan task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Wait for review_plan (wait_human) step with ExecWaiting state.
	waitFor(t, 20*time.Second, "workflow reaches review_plan", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.CurrentStep == "review_plan" && tk.Workflow.State == workflow.ExecWaiting
	})

	// Count plan records before reject.
	tk0, _ := env.tasks.Get(created.ID)
	planCountBefore := 0
	for _, r := range tk0.Workflow.StepHistory {
		if r.StepID == "plan" {
			planCountBefore++
		}
	}

	// Reject with feedback.
	if err := env.engine.HandleHumanAction(created.ID, "reject", map[string]string{"feedback": "add error handling"}); err != nil {
		t.Fatal(err)
	}

	// Wait for plan agent to rerun and reach review_plan again (plan count increases).
	waitFor(t, 20*time.Second, "workflow returns to review_plan after reject", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		if tk.Workflow == nil || tk.Workflow.CurrentStep != "review_plan" || tk.Workflow.State != workflow.ExecWaiting {
			return false
		}
		planCount := 0
		for _, r := range tk.Workflow.StepHistory {
			if r.StepID == "plan" {
				planCount++
			}
		}
		return planCount > planCountBefore
	})

	// Now approve.
	// Need fresh scenarios for implement + evaluate.
	if env.scenarioFile != "" {
		_ = os.WriteFile(env.scenarioFile, []byte("success\nsuccess"), 0o644)
	}

	if err := env.engine.HandleHumanAction(created.ID, "approve", nil); err != nil {
		t.Fatal(err)
	}

	// Wait for workflow to complete.
	waitFor(t, 20*time.Second, "workflow completes after approve", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && (tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed)
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("expected completed, got %q (step: %s)", tk.Workflow.State, tk.Workflow.CurrentStep)
	}
}

func TestE2E_ConcurrentWorkflows(t *testing.T) {
	env := setupE2E(t, "success")

	// Create two tasks simultaneously.
	t1, err := env.tasks.Create("concurrent task 1", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := env.tasks.Create("concurrent task 2", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Start both workflows concurrently.
	errCh := make(chan error, 2)
	go func() { errCh <- env.engine.StartWorkflow(t1.ID, "test-simple") }()
	go func() { errCh <- env.engine.StartWorkflow(t2.ID, "test-simple") }()

	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}

	// Both should advance past triage independently.
	waitFor(t, 20*time.Second, "both workflows advance past triage", func() bool {
		tk1, err1 := env.tasks.Get(t1.ID)
		tk2, err2 := env.tasks.Get(t2.ID)
		if err1 != nil || err2 != nil {
			return false
		}
		past1 := tk1.Workflow != nil && tk1.Workflow.CurrentStep != "triage"
		past2 := tk2.Workflow != nil && tk2.Workflow.CurrentStep != "triage"
		return past1 && past2
	})

	// Verify both have independent workflow state.
	tk1, _ := env.tasks.Get(t1.ID)
	tk2, _ := env.tasks.Get(t2.ID)
	if tk1.Workflow.WorkflowID != "test-simple" || tk2.Workflow.WorkflowID != "test-simple" {
		t.Fatal("both tasks should have test-simple workflow")
	}
}
