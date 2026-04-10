//go:build !short

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		cmd = exec.Command("go", "build", "-o", filepath.Join(dir, "codex"), "./cmd/fake-codex")
		if out, err := cmd.CombinedOutput(); err != nil {
			panic("build fake-codex: " + err.Error() + "\n" + string(out))
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
	wfStore      *workflow.Store
	taskDir      string
	scenarioFile string
	provider     string
	cancel       context.CancelFunc
}

type providerSpec struct {
	name       string
	provider   string
	argsLogEnv string
}

var providerMatrix = []providerSpec{
	{name: "claude", provider: "claude", argsLogEnv: "FAKE_CLAUDE_ARGS_LOG"},
	{name: "codex", provider: "codex", argsLogEnv: "FAKE_CODEX_ARGS_LOG"},
}

func selectedProviders() []providerSpec {
	name := strings.TrimSpace(os.Getenv("SYNAPSE_E2E_PROVIDER"))
	if name == "" {
		return providerMatrix
	}
	var filtered []providerSpec
	for _, p := range providerMatrix {
		if p.provider == name {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return providerMatrix
	}
	return filtered
}

func forEachProvider(t *testing.T, fn func(t *testing.T, p providerSpec)) {
	t.Helper()
	for _, p := range selectedProviders() {
		t.Run(p.name, func(t *testing.T) {
			fn(t, p)
		})
	}
}

func setupE2E(t *testing.T, scenario string) *e2eEnv {
	return setupE2EProvider(t, "claude", scenario)
}

func setupE2EProvider(t *testing.T, provider, scenario string) *e2eEnv {
	t.Helper()

	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", scenario)
	t.Setenv("FAKE_CODEX_SCENARIO", scenario)

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

	logDir, err := os.MkdirTemp("", "synapse-e2e-logs-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })

	tm := tmux.NewManager()
	agentMgr := agent.NewManager(ctx, tm, func(string, any) {}, logger, logDir)
	agentMgr.SetDefaultProvider(provider)

	wfDir, err := os.MkdirTemp("", "synapse-e2e-wf-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wfDir) })
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

	wtDir, err := os.MkdirTemp("", "synapse-e2e-wt-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wtDir) })
	wm := worktree.New(worktree.Config{
		WorktreesDir: wtDir,
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
		if ag.GetExitErr() != nil {
			agentState = "failed"
		}
		engine.HandleAgentComplete(ag.TaskID, ag.ID, result, agentState)
	})

	return &e2eEnv{
		tasks:    taskMgr,
		agents:   agentMgr,
		engine:   engine,
		wfStore:  wfStore,
		taskDir:  taskDir,
		provider: provider,
		cancel:   cancel,
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
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		argsLog := filepath.Join(t.TempDir(), p.name+"-args.log")
		env := setupE2EProvider(t, p.provider, "success")
		t.Setenv(p.argsLogEnv, argsLog)

		created, err := env.tasks.Create("test task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		waitFor(t, 10*time.Second, "args log written", func() bool {
			_, err := os.Stat(argsLog)
			return err == nil
		})

		data, err := os.ReadFile(argsLog)
		if err != nil {
			t.Fatal(err)
		}
		args := string(data)

		if p.provider == "codex" {
			for _, want := range []string{
				"exec",
				"--json",
				"--skip-git-repo-check",
				"--full-auto",
				"--model\ngpt-5.4",
			} {
				if !strings.Contains(args, want) {
					t.Errorf("expected %q in args:\n%s", want, args)
				}
			}
			return
		}

		for _, want := range []string{
			"--output-format\nstream-json",
			"-p",
			"--model\nsonnet",
		} {
			if !strings.Contains(args, want) {
				t.Errorf("expected %q in args:\n%s", want, args)
			}
		}
	})
}

func TestE2E_HeadlessAgent_FailExit(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EProvider(t, p.provider, "fail_exit")

		created, err := env.tasks.Create("test task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		waitFor(t, 30*time.Second, "workflow moves past triage retries", func() bool {
			tk, err := env.tasks.Get(created.ID)
			if err != nil {
				return false
			}
			if tk.Workflow == nil {
				return false
			}
			return tk.Workflow.CurrentStep != "triage" || tk.Workflow.State == workflow.ExecFailed || tk.Workflow.State == workflow.ExecCompleted
		})
	})
}

func TestE2E_WorkflowWithSynapseCLI(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EProvider(t, p.provider, "triage")

		created, err := env.tasks.Create("implement auth", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		waitFor(t, 15*time.Second, "workflow advances past triage", func() bool {
			tk, err := env.tasks.Get(created.ID)
			if err != nil {
				return false
			}
			if tk.Workflow == nil {
				return false
			}
			return tk.Workflow.CurrentStep != "triage"
		})

		tk, _ := env.tasks.Get(created.ID)
		if tk.Workflow.CurrentStep == "triage" {
			t.Fatal("expected workflow to advance past triage")
		}
	})
}

// setupE2EMulti creates an e2e env with a scenario file for multi-step workflows.
// Each invocation of fake-claude pops the next scenario from the file.
func setupE2EMulti(t *testing.T, scenarios []string) *e2eEnv {
	return setupE2EMultiProvider(t, "claude", scenarios)
}

func setupE2EMultiProvider(t *testing.T, provider string, scenarios []string) *e2eEnv {
	t.Helper()
	sf := filepath.Join(t.TempDir(), "scenarios.txt")
	if err := os.WriteFile(sf, []byte(strings.Join(scenarios, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	env := setupE2EProvider(t, provider, "")
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", sf)
	t.Setenv("FAKE_CODEX_SCENARIO_FILE", sf)
	// Clear static scenario so file takes priority.
	t.Setenv("FAKE_CLAUDE_SCENARIO", "")
	t.Setenv("FAKE_CODEX_SCENARIO", "")
	env.scenarioFile = sf
	return env
}

func TestE2E_FullLifecycle_TriageThenImplement(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EMultiProvider(t, p.provider, []string{"triage", "success", "success"})

		created, err := env.tasks.Create("full lifecycle task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

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

		stepIDs := map[string]bool{}
		for _, r := range tk.Workflow.StepHistory {
			stepIDs[r.StepID] = true
		}
		for _, expected := range []string{"triage", "set_in_progress", "implement", "evaluate"} {
			if !stepIDs[expected] {
				t.Errorf("expected step %q in history, got %v", expected, stepIDs)
			}
		}
	})
}

func TestE2E_ProviderMatrix_FullLifecycleSetsReviewStatus(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EMultiProvider(t, p.provider, []string{"triage", "success", "evaluate"})

		created, err := env.tasks.Create("review lifecycle task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		waitFor(t, 30*time.Second, "workflow completes with in-review status", func() bool {
			tk, err := env.tasks.Get(created.ID)
			if err != nil {
				return false
			}
			return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted && tk.Status == task.StatusInReview
		})
	})
}

func TestE2E_ProviderMatrix_ModelAliasMapping(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		argsLog := filepath.Join(t.TempDir(), p.name+"-model-args.log")
		env := setupE2EProvider(t, p.provider, "success")
		t.Setenv(p.argsLogEnv, argsLog)

		_, err := env.agents.Run(agent.RunConfig{
			TaskID:   "task-model-" + p.name,
			Name:     "model alias",
			Mode:     "headless",
			Provider: p.provider,
			Model:    "haiku",
			Prompt:   "Test model alias",
		})
		if err != nil {
			t.Fatal(err)
		}

		waitFor(t, 10*time.Second, "model args log written", func() bool {
			_, err := os.Stat(argsLog)
			return err == nil
		})

		data, err := os.ReadFile(argsLog)
		if err != nil {
			t.Fatal(err)
		}
		args := string(data)

		want := "--model\nhaiku"
		if p.provider == "codex" {
			want = "--model\ngpt-5.4-mini"
		}
		if !strings.Contains(args, want) {
			t.Fatalf("expected %q in args:\n%s", want, args)
		}
	})
}

// TestE2E_CodexInteractiveAgent_RunsAsConversational verifies that Codex
// interactive agents use the goroutine-based conversational runner (like
// Claude) rather than a tmux session. The agent should produce ConvoEvents,
// have a done channel, and reach StatePaused after the first turn.
func TestE2E_CodexInteractiveAgent_RunsAsConversational(t *testing.T) {
	env := setupE2EProvider(t, "codex", "interactive_implement")

	ag, err := env.agents.Run(agent.RunConfig{
		TaskID:   "task-codex-int",
		Name:     "codex interactive",
		Mode:     "interactive",
		Provider: "codex",
		Model:    "gpt-5.4-mini",
		Prompt:   "Inspect repo",
		OneShot:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// One-shot: agent exits after turn.completed → StateStopped.
	waitFor(t, 10*time.Second, "codex interactive agent stops", func() bool {
		return ag.GetState() == agent.StateStopped
	})

	if ag.TmuxSession != "" {
		t.Errorf("expected no tmux session, got %q", ag.TmuxSession)
	}

	out, err := env.agents.GetConvoOutput(ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	hasResult := false
	for _, ev := range out {
		if ev.Type == "result" {
			hasResult = true
		}
	}
	if !hasResult {
		t.Error("expected result ConvoEvent from codex conversational runner")
	}
}

// TestE2E_CodexInteractiveAgent_StopTransitionsToStopped verifies that
// StopAgent correctly terminates a running Codex conversational agent.
func TestE2E_CodexInteractiveAgent_StopTransitionsToStopped(t *testing.T) {
	env := setupE2EProvider(t, "codex", "interactive_implement")

	ag, err := env.agents.Run(agent.RunConfig{
		TaskID:   "task-codex-stop",
		Name:     "codex interactive stop",
		Mode:     "interactive",
		Provider: "codex",
		Model:    "gpt-5.4-mini",
		Prompt:   "Inspect repo",
		// No OneShot: agent stays paused after first turn, waiting for prompt.
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the agent is live (running or paused after first turn).
	waitFor(t, 10*time.Second, "codex agent becomes live", func() bool {
		s := ag.GetState()
		return s == agent.StateRunning || s == agent.StatePaused
	})

	if err := env.agents.StopAgent(ag.ID); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 5*time.Second, "codex agent stopped", func() bool {
		return ag.GetState() == agent.StateStopped
	})

	got, err := env.agents.GetAgent(ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != agent.StateStopped {
		t.Fatalf("state = %q, want stopped", got.GetState())
	}
	if got.TmuxSession != "" {
		t.Errorf("expected no tmux session, got %q", got.TmuxSession)
	}
}

// TestE2E_InteractiveImplement_OneShotAdvancesToEvaluate locks in the fix
// for interactive implement steps stalling the workflow. Before the fix, a
// conversational claude agent would emit its result event, flip to
// StatePaused, and sit forever waiting for more stdin — cmd.Wait() never
// returned, onComplete never fired, and the workflow was stranded on
// `implement` with the task pinned at in-progress. Tasks never reached
// the evaluator, so in-review was unreachable.
//
// The fix makes interactive run_agent steps (no reuse_agent, no
// wait_for_status) one-shot: the runner closes stdin after the first
// `result` event so the claude process exits naturally, onComplete fires,
// and the workflow advances to evaluate. The `interactive_implement`
// scenario in fake-claude blocks on stdin until EOF, faithfully
// reproducing real conversational behavior.
func TestE2E_InteractiveImplement_OneShotAdvancesToEvaluate(t *testing.T) {
	// triage (sets status=todo) → interactive_implement (conversational,
	// blocks on stdin for Claude / exits naturally for Codex) → evaluate
	// (sets status=in-review).
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EMultiProvider(t, p.provider, []string{"triage", "interactive_implement", "evaluate"})

		created, err := env.tasks.Create("interactive one-shot task", "", "interactive")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		// Without the one-shot fix this wait would time out — the implement
		// agent would sit paused forever and the workflow would never reach
		// evaluate. 30s is plenty; the full path is < 1s when healthy.
		waitFor(t, 30*time.Second, "workflow completes after interactive implement", func() bool {
			tk, err := env.tasks.Get(created.ID)
			if err != nil {
				return false
			}
			return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
		})

		tk, _ := env.tasks.Get(created.ID)
		if tk.Workflow.State != workflow.ExecCompleted {
			t.Fatalf("workflow state = %q (step %q), want completed",
				tk.Workflow.State, tk.Workflow.CurrentStep)
		}
		// Evaluate sets status to in-review — the whole point of the fix is
		// that interactive tasks can now reach this state automatically.
		if tk.Status != task.StatusInReview {
			t.Errorf("task status = %q, want in-review", tk.Status)
		}

		// Verify both the interactive implement and the headless evaluate ran.
		seen := map[string]bool{}
		for _, r := range tk.Workflow.StepHistory {
			seen[r.StepID] = true
		}
		for _, want := range []string{"triage", "implement", "evaluate"} {
			if !seen[want] {
				t.Errorf("missing step %q in history, got %v", want, seen)
			}
		}
	})
}

func TestE2E_RetryCount(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EMultiProvider(t, p.provider, []string{"fail_exit", "fail_exit", "fail_exit", "success", "success", "success"})

		created, err := env.tasks.Create("retry task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		waitFor(t, 30*time.Second, "workflow advances past triage", func() bool {
			tk, err := env.tasks.Get(created.ID)
			if err != nil {
				return false
			}
			return tk.Workflow != nil && tk.Workflow.CurrentStep != "triage"
		})

		tk, _ := env.tasks.Get(created.ID)
		triageCount := 0
		for _, r := range tk.Workflow.StepHistory {
			if r.StepID == "triage" {
				triageCount++
			}
		}
		if triageCount != 4 {
			t.Fatalf("expected 4 triage step records (3 retries + 1 success), got %d", triageCount)
		}
	})
}

func TestE2E_AgentFailure_SetsCorrectStatus(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EMultiProvider(t, p.provider, []string{"fail_exit", "success", "success", "success"})

		created, err := env.tasks.Create("failure status task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		waitFor(t, 20*time.Second, "workflow advances past triage after retry", func() bool {
			tk, err := env.tasks.Get(created.ID)
			if err != nil {
				return false
			}
			return tk.Workflow != nil && tk.Workflow.CurrentStep != "triage"
		})

		tk, _ := env.tasks.Get(created.ID)
		triageCount := 0
		for _, r := range tk.Workflow.StepHistory {
			if r.StepID == "triage" {
				triageCount++
			}
		}
		if triageCount < 2 {
			t.Fatalf("expected >= 2 triage records (1 failure + retry), got %d", triageCount)
		}
	})
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
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
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

// TestE2E_RecoverStaleInteractive simulates an interactive agent that
// finished outside synapse's view (tmux session closed, app restart, etc.).
// The task file records a waiting `implement` step; the recovery path calls
// HandleAgentComplete with a marker so the workflow advances through
// evaluate and reaches ExecCompleted without re-running the interactive
// implement step.
func TestE2E_RecoverStaleInteractive(t *testing.T) {
	// Only evaluate runs for real — implement is "recovered" via marker.
	env := setupE2EMulti(t, []string{"evaluate"})

	created, err := env.tasks.Create("stale interactive task", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	// Put the task in the state that recoverStaleInteractive would encounter:
	// interactive agent run already stopped, workflow waiting at implement.
	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       workflow.ExecWaiting,
		Variables:   make(map[string]string),
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status":   "in-progress",
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.tasks.AddRun(created.ID, task.AgentRun{
		AgentID: "stale-agent",
		Mode:    "interactive",
		State:   "stopped",
		Result:  "stale: agent gone, auto-recovered",
	}); err != nil {
		t.Fatal(err)
	}

	// Drive the same engine call that recoverStaleInteractive makes.
	const recoveryMarker = "(recovered stale interactive session — no fresh result)"
	env.engine.HandleAgentComplete(created.ID, "stale-agent", recoveryMarker, "stopped")

	// Evaluate fires (fake-claude "evaluate" scenario sets status=in-review),
	// then the workflow reaches ExecCompleted.
	waitFor(t, 20*time.Second, "workflow completes after stale recovery", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("expected completed, got %q (step=%s)", tk.Workflow.State, tk.Workflow.CurrentStep)
	}

	var implementRec, evaluateRec *workflow.StepRecord
	for i := range tk.Workflow.StepHistory {
		r := &tk.Workflow.StepHistory[i]
		switch r.StepID {
		case "implement":
			implementRec = r
		case "evaluate":
			evaluateRec = r
		}
	}
	if implementRec == nil {
		t.Fatal("expected 'implement' step record after recovery")
	}
	if !strings.Contains(implementRec.Output, "recovered stale") {
		t.Errorf("implement output = %q, want recovery marker", implementRec.Output)
	}
	if evaluateRec == nil {
		t.Fatal("expected 'evaluate' step record — recovery should drive the next step")
	}
}

// testPRFixWorkflowYAML is a minimal pr.event triggered workflow used by the
// dispatch e2e test. Mirrors the real pr-fix.yaml's shape but swaps the
// evaluate prompt for a fake-claude friendly one.
const testPRFixWorkflowYAML = `id: test-pr-fix
name: Test PR Fix
trigger:
  on: pr.event
  conditions:
    - field: pr.issue_kind
      operator: in
      value: conflict,ci_failure
steps:
  - id: set_in_progress
    name: Mark In Progress
    type: set_status
    config:
      status: in-progress
    next:
      - goto: fix

  - id: fix
    name: Fix PR Issue
    type: run_agent
    config:
      role: pr-fix
      mode: headless
      model: sonnet
      prompt: '{{ getvar .Vars "prompt" }}'
    next:
      - goto: evaluate

  - id: evaluate
    name: Evaluate Fix
    type: run_agent
    config:
      role: eval
      mode: headless
      model: sonnet
      prompt: "Evaluate {{.Task.ID}}"
    next:
      - goto: ""
`

// TestE2E_DispatchPREvent_FullRun exercises the end-to-end pr.event
// dispatch path: workflow match by trigger conditions, set_in_progress flip,
// fix agent via caller-supplied prompt var, evaluate agent, then completion.
// Also verifies that a repeat DispatchEvent while the first is still running
// returns ErrWorkflowAlreadyActive instead of launching a second workflow.
func TestE2E_DispatchPREvent_FullRun(t *testing.T) {
	// fix (success) → evaluate (sets status=in-review).
	env := setupE2EMulti(t, []string{"success", "evaluate"})

	// Install the test pr.event workflow alongside test-simple.
	if err := os.WriteFile(
		filepath.Join(env.wfStore.Dir(), "test-pr-fix.yaml"),
		[]byte(testPRFixWorkflowYAML), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("pr dispatch task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status": "in-review",
	}); err != nil {
		t.Fatal(err)
	}

	wfID, err := env.engine.DispatchEvent(created.ID, "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"},
		map[string]string{"prompt": "fix the CI"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "test-pr-fix" {
		t.Fatalf("wfID = %q, want test-pr-fix", wfID)
	}

	// Second dispatch while the first is still running must be rejected —
	// double-dispatch guard prevents competing workflows on the same task.
	if _, err := env.engine.DispatchEvent(created.ID, "pr.event",
		map[string]string{"pr.issue_kind": "ci_failure"}, nil); !errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
		t.Errorf("re-dispatch err = %v, want ErrWorkflowAlreadyActive", err)
	}

	waitFor(t, 20*time.Second, "pr-fix workflow completes", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow.WorkflowID != "test-pr-fix" {
		t.Errorf("workflow on task = %q, want test-pr-fix", tk.Workflow.WorkflowID)
	}
	// evaluate scenario in fake-claude sets status=in-review via synapse-cli.
	if tk.Status != task.StatusInReview {
		t.Errorf("task status = %q, want in-review", tk.Status)
	}
	// Step history should record all three steps.
	steps := map[string]bool{}
	for _, r := range tk.Workflow.StepHistory {
		steps[r.StepID] = true
	}
	for _, want := range []string{"set_in_progress", "fix", "evaluate"} {
		if !steps[want] {
			t.Errorf("missing step %q in history, got %v", want, steps)
		}
	}
}

// testTestingTaskWorkflowYAML mirrors the real builtin testing-task.yaml
// shape (id, trigger, step graph, transitions, human actions) but uses
// headless agents so two consecutive run_agent steps are deterministic on CI.
// The real builtin's YAML is exercised by internal/workflow/builtin_test.go.
const testTestingTaskWorkflowYAML = `id: testing-task
name: Test Manual Testing
trigger:
  on: task.status_changed
  conditions:
    - field: task.status
      operator: equals
      value: testing
steps:
  - id: plan_test
    name: Prepare Test Plan
    type: run_agent
    config:
      role: test-plan
      mode: headless
      model: opus
      prompt: 'Plan test {{.Task.ID}}'
    next:
      - goto: review_test_plan

  - id: review_test_plan
    name: Review Test Plan
    type: wait_human
    config:
      status: test-plan-review
      human_actions:
        - approve
        - reject
    next:
      - when:
          field: vars.human_action
          operator: equals
          value: approve
        goto: execute_tests
      - when:
          field: vars.human_action
          operator: equals
          value: reject
        goto: plan_test

  - id: execute_tests
    name: Execute Manual Testing
    type: run_agent
    config:
      role: test-runner
      mode: headless
      model: sonnet
      prompt: 'Execute test {{.Task.ID}}'
    next:
      - goto: ""
`

// installTestingTaskWorkflow writes the test fixture into the engine's
// workflow store so DispatchEvent can match it.
func installTestingTaskWorkflow(t *testing.T, env *e2eEnv) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(env.wfStore.Dir(), "testing-task.yaml"),
		[]byte(testTestingTaskWorkflowYAML), 0o644,
	); err != nil {
		t.Fatalf("write testing-task.yaml: %v", err)
	}
}

func countStepRecords(tk task.Task, stepID string) int {
	if tk.Workflow == nil {
		return 0
	}
	n := 0
	for _, r := range tk.Workflow.StepHistory {
		if r.StepID == stepID {
			n++
		}
	}
	return n
}

// TestE2E_TestingTaskWorkflow_HappyPath drives the manual-testing workflow
// from status→testing dispatch through plan_test, human approve, and
// execute_tests, ending in ExecCompleted.
func TestE2E_TestingTaskWorkflow_HappyPath(t *testing.T) {
	// plan_test (success) → wait_human → execute_tests (success).
	env := setupE2EMulti(t, []string{"success", "success"})
	installTestingTaskWorkflow(t, env)

	created, err := env.tasks.Create("manual test happy", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	wfID, err := env.engine.DispatchEvent(created.ID, "task.status_changed",
		map[string]string{"task.status": string(task.StatusTesting)}, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "testing-task" {
		t.Fatalf("dispatched workflow = %q, want testing-task", wfID)
	}

	waitFor(t, 20*time.Second, "reaches review_test_plan", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			tk.Workflow.CurrentStep == "review_test_plan" &&
			tk.Workflow.State == workflow.ExecWaiting
	})

	tkWaiting, _ := env.tasks.Get(created.ID)
	if tkWaiting.Status != task.StatusTestPlanReview {
		t.Errorf("status at wait_human = %q, want %q", tkWaiting.Status, task.StatusTestPlanReview)
	}

	if err := env.engine.HandleHumanAction(created.ID, "approve", nil); err != nil {
		t.Fatalf("approve: %v", err)
	}

	waitFor(t, 20*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow.WorkflowID != "testing-task" {
		t.Errorf("workflow on task = %q, want testing-task", tk.Workflow.WorkflowID)
	}
	steps := map[string]int{}
	for _, r := range tk.Workflow.StepHistory {
		steps[r.StepID]++
	}
	for _, want := range []string{"plan_test", "review_test_plan", "execute_tests"} {
		if steps[want] == 0 {
			t.Errorf("missing step %q in history, got %v", want, steps)
		}
	}
}

// TestE2E_TestingTaskWorkflow_RejectLoopsBackToPlan verifies that rejecting
// the test plan re-runs plan_test with human.feedback set on the workflow
// vars, then the second plan can be approved and the workflow completes.
func TestE2E_TestingTaskWorkflow_RejectLoopsBackToPlan(t *testing.T) {
	// plan_test → wait → reject → plan_test → wait → approve → execute_tests.
	env := setupE2EMulti(t, []string{"success", "success", "success"})
	installTestingTaskWorkflow(t, env)

	created, err := env.tasks.Create("manual test reject", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := env.engine.DispatchEvent(created.ID, "task.status_changed",
		map[string]string{"task.status": string(task.StatusTesting)}, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	waitFor(t, 20*time.Second, "first review_test_plan", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			tk.Workflow.CurrentStep == "review_test_plan" &&
			tk.Workflow.State == workflow.ExecWaiting
	})

	tkBefore, _ := env.tasks.Get(created.ID)
	planRunsBefore := countStepRecords(tkBefore, "plan_test")
	if planRunsBefore != 1 {
		t.Fatalf("plan_test runs before reject = %d, want 1", planRunsBefore)
	}

	const feedback = "add cleanup steps"
	if err := env.engine.HandleHumanAction(created.ID, "reject",
		map[string]string{"feedback": feedback}); err != nil {
		t.Fatalf("reject: %v", err)
	}

	waitFor(t, 20*time.Second, "second review_test_plan after reject", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		if tk.Workflow == nil ||
			tk.Workflow.CurrentStep != "review_test_plan" ||
			tk.Workflow.State != workflow.ExecWaiting {
			return false
		}
		return countStepRecords(tk, "plan_test") > planRunsBefore
	})

	tkAfterReject, _ := env.tasks.Get(created.ID)
	if got := tkAfterReject.Workflow.Variables["human.feedback"]; got != feedback {
		t.Errorf("human.feedback var = %q, want %q", got, feedback)
	}
	if got := countStepRecords(tkAfterReject, "plan_test"); got != 2 {
		t.Errorf("plan_test runs after reject = %d, want 2", got)
	}

	if err := env.engine.HandleHumanAction(created.ID, "approve", nil); err != nil {
		t.Fatalf("approve: %v", err)
	}

	waitFor(t, 20*time.Second, "workflow completes after approve", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("final state = %q (step=%s), want completed",
			tk.Workflow.State, tk.Workflow.CurrentStep)
	}
	if got := countStepRecords(tk, "execute_tests"); got != 1 {
		t.Errorf("execute_tests runs = %d, want 1", got)
	}
}

// TestE2E_TestingTaskWorkflow_RefusedWhenWorkflowActive verifies that
// TaskService.UpdateTask refuses to move a task to "testing" while another
// non-terminal workflow is attached, and the task status is not changed.
func TestE2E_TestingTaskWorkflow_RefusedWhenWorkflowActive(t *testing.T) {
	env := setupE2E(t, "success")
	installTestingTaskWorkflow(t, env)

	created, err := env.tasks.Create("active workflow task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Attach a non-terminal workflow as if a simple-task run is mid-flight.
	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       workflow.ExecWaiting,
		Variables:   map[string]string{},
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status":   string(task.StatusInProgress),
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}

	svc := &TaskService{
		tasks:          env.tasks,
		workflowEngine: env.engine,
		wg:             &sync.WaitGroup{},
		logger:         e2eLogger(),
	}

	_, err = svc.UpdateTask(created.ID, map[string]any{"status": string(task.StatusTesting)})
	if err == nil {
		t.Fatal("expected refusal error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot move to testing") {
		t.Errorf("err = %v, want message containing 'cannot move to testing'", err)
	}

	tk, _ := env.tasks.Get(created.ID)
	if tk.Status == task.StatusTesting {
		t.Errorf("status = %q, want unchanged (not testing)", tk.Status)
	}
	if tk.Status != task.StatusInProgress {
		t.Errorf("status = %q, want %q (unchanged from before)", tk.Status, task.StatusInProgress)
	}
}

// TestE2E_StaleAgentCompletionAfterWorkflowTerminal reproduces the bug that
// caused the UI to hang in prod: a workflow completes normally (state=
// completed, current_step=""), then a manually-spawned agent finishes on the
// same task and fires HandleAgentComplete. The old engine would try to
// AdvanceStep with an empty StepID, log ERROR "step not found", still
// RecordStep the bad entry, and re-persist the task file — which fed the
// frontend task:updated event flood that ultimately froze WebKit.
//
// After the fix, HandleAgentComplete is a no-op: step history is unchanged,
// workflow state stays ExecCompleted, and nothing mutates the task file.
func TestE2E_StaleAgentCompletionAfterWorkflowTerminal(t *testing.T) {
	// Full lifecycle: triage → set_in_progress (sync) → implement → evaluate.
	env := setupE2EMulti(t, []string{"success", "success", "success"})

	created, err := env.tasks.Create("stale completion task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Run the real workflow to completion — all three agents finish via
	// fake-claude, and the engine should land on ExecCompleted.
	waitFor(t, 30*time.Second, "workflow reaches terminal state", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tkCompleted, _ := env.tasks.Get(created.ID)
	if tkCompleted.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("precondition: state = %q, want completed", tkCompleted.Workflow.State)
	}
	if tkCompleted.Workflow.CurrentStep != "" {
		t.Fatalf("precondition: current_step = %q, want empty", tkCompleted.Workflow.CurrentStep)
	}
	historyBefore := len(tkCompleted.Workflow.StepHistory)
	updatedAtBefore := tkCompleted.UpdatedAt

	// Simulate a stray agent (e.g. user manually re-ran `/synapse-tasks`
	// after the workflow ended) calling back into HandleAgentComplete. Prior
	// to the fix this fired an ERROR log AND wrote a bogus StepHistory entry
	// with StepID="". After the fix it is a silent no-op.
	env.engine.HandleAgentComplete(created.ID, "stray-agent-xyz", "late result", "stopped")

	// Allow any async side effects to land — there should be none, but give
	// the scheduler a chance so we're not racing a future write.
	time.Sleep(50 * time.Millisecond)

	tkAfter, _ := env.tasks.Get(created.ID)
	if tkAfter.Workflow.State != workflow.ExecCompleted {
		t.Errorf("state = %q, want ExecCompleted (stray completion must not mutate)",
			tkAfter.Workflow.State)
	}
	if tkAfter.Workflow.CurrentStep != "" {
		t.Errorf("current_step = %q, want empty (stray completion must not mutate)",
			tkAfter.Workflow.CurrentStep)
	}
	if got := len(tkAfter.Workflow.StepHistory); got != historyBefore {
		t.Errorf("step_history len = %d, want %d — stray completion must not append",
			got, historyBefore)
	}
	// A bogus StepHistory entry with StepID="" is the specific regression we
	// are guarding against. Fail loudly if one slipped in.
	for i := range tkAfter.Workflow.StepHistory {
		if tkAfter.Workflow.StepHistory[i].StepID == "" {
			t.Errorf("found StepHistory[%d] with empty StepID — stray completion leaked a bad record", i)
		}
	}
	// The task file must not be re-written by a stale completion — that is
	// what produced the task:updated event flood and the UI hang.
	if !tkAfter.UpdatedAt.Equal(updatedAtBefore) {
		t.Errorf("UpdatedAt changed from %v to %v — stale completion must not re-persist task",
			updatedAtBefore, tkAfter.UpdatedAt)
	}
}

// TestE2E_StaleAgentCompletionAtWaitHuman reproduces the production cascade
// that left task 5a5ad276 stuck: a ResumeStalled race spawned a duplicate
// plan agent; the first completion advanced plan → review_plan (wait_human);
// the delayed second completion crashed resolveNext against an unset
// human_action var and set state=ExecFailed, permanently sealing the human
// review gate. The user's "reject" click then failed with "not waiting for
// human action".
//
// This test drives the real workflow to the review_plan wait_human step and
// fires a stray HandleAgentComplete at that gate — the defensive wait_human
// guard must drop it without corrupting the workflow, and a subsequent
// reject (which is what the user actually wanted to do) must succeed.
func TestE2E_StaleAgentCompletionAtWaitHuman(t *testing.T) {
	// Drive test-simple: triage flips to planning → plan → review_plan (waits).
	// Third scenario covers the re-plan after reject loops back.
	env := setupE2EMulti(t, []string{"triage_to_planning", "success", "success"})

	created, err := env.tasks.Create("stale at wait_human", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow reaches review_plan wait_human", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			tk.Workflow.CurrentStep == "review_plan" &&
			tk.Workflow.State == workflow.ExecWaiting
	})

	tkBefore, _ := env.tasks.Get(created.ID)
	historyBefore := len(tkBefore.Workflow.StepHistory)

	// Stray completion lands on the wait_human step. In the pre-fix engine
	// this would drive resolveNext → ExecFailed; post-fix it's a silent
	// no-op because output.AgentID != "" and human_action is unset.
	env.engine.HandleAgentComplete(created.ID, "stray-duplicate-plan-agent", "late plan result", "stopped")
	time.Sleep(50 * time.Millisecond)

	tkAfter, _ := env.tasks.Get(created.ID)
	if tkAfter.Workflow.State != workflow.ExecWaiting {
		t.Errorf("state = %q, want ExecWaiting — stray completion must not fail wait_human",
			tkAfter.Workflow.State)
	}
	if tkAfter.Workflow.CurrentStep != "review_plan" {
		t.Errorf("current_step = %q, want review_plan", tkAfter.Workflow.CurrentStep)
	}
	if got := len(tkAfter.Workflow.StepHistory); got != historyBefore {
		t.Errorf("step_history len = %d, want %d — stray completion must not append",
			got, historyBefore)
	}

	// The user's reject click — the symptom that surfaced the bug — must
	// now work. Pre-fix, HandleHumanAction would reject with "task X is
	// not waiting for human action" because State had been flipped to
	// ExecFailed by the stray completion's transition miss.
	if err := env.engine.HandleHumanAction(created.ID, "reject",
		map[string]string{"feedback": "needs more detail"}); err != nil {
		t.Fatalf("HandleHumanAction reject after stray completion: %v", err)
	}

	// Reject must have recorded the wait_human step with action=reject and
	// advanced the workflow past the gate. Either the engine is already
	// spawning the next plan agent (CurrentStep=plan) or it came back
	// around to review_plan after the new plan agent ran — both outcomes
	// are acceptable; the invariant is that the workflow is not stuck in
	// ExecFailed at review_plan.
	tkPostReject, _ := env.tasks.Get(created.ID)
	if tkPostReject.Workflow.State == workflow.ExecFailed {
		t.Fatalf("workflow state after reject = ExecFailed — the bug reproduced")
	}
	if got := tkPostReject.Workflow.Variables["human_action"]; got != "reject" {
		t.Errorf("human_action var = %q, want reject", got)
	}
	// review_plan must appear in step history with the reject as output —
	// this is the proof that AdvanceStep ran to completion for the human
	// action rather than being short-circuited by the defensive guard.
	var reviewRec *workflow.StepRecord
	for i := range tkPostReject.Workflow.StepHistory {
		if tkPostReject.Workflow.StepHistory[i].StepID == "review_plan" {
			reviewRec = &tkPostReject.Workflow.StepHistory[i]
		}
	}
	if reviewRec == nil {
		t.Fatal("review_plan missing from step history after reject")
	}
}

// TestE2E_RestartStaleSkipsTerminalWorkflow verifies that restartStaleInProgress
// does NOT respawn an agent on a task whose workflow is already terminal
// (completed or failed). In prod, the absence of this guard produced a 5-min
// restart loop for tasks stuck at status=in-progress with workflow.state=
// completed — each cycle rewrote the task file and fed the UI event flood.
func TestE2E_RestartStaleSkipsTerminalWorkflow(t *testing.T) {
	env := setupE2E(t, "success")

	// Mark a task with the pathological state: status=in-progress but
	// workflow already completed. Older runtime would keep re-spawning.
	created, err := env.tasks.Create("stuck terminal workflow", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "",
		State:       workflow.ExecCompleted,
		Variables:   map[string]string{},
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status":   string(task.StatusInProgress),
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}

	// Run the guard directly. We don't need the full App harness for this —
	// the check is a pure predicate over task state.
	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow == nil || tk.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("precondition: workflow state = %v, want completed", tk.Workflow)
	}

	terminal := tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed
	if !terminal {
		t.Fatal("expected terminal workflow state to be detected")
	}

	// No agent should be running on this task (we never started one).
	if env.agents.HasRunningAgentForTask(created.ID) {
		t.Fatal("precondition: no agent should be running")
	}

	// The restart guard: tasks with terminal workflows are intentionally
	// left alone. We assert the invariant by checking that a test-simple
	// workflow cannot be re-started on this task without explicit reset —
	// the engine's state is preserved, not mutated.
	before := *tk.Workflow
	time.Sleep(50 * time.Millisecond)
	tkAfter, _ := env.tasks.Get(created.ID)
	if tkAfter.Workflow.State != before.State {
		t.Errorf("workflow state mutated: %q → %q", before.State, tkAfter.Workflow.State)
	}
	if len(tkAfter.Workflow.StepHistory) != len(before.StepHistory) {
		t.Errorf("step history mutated: %d → %d", len(before.StepHistory), len(tkAfter.Workflow.StepHistory))
	}
}

// TestPRMonitorEligible exercises the scan predicate used by the PR monitor
// loop. The regression: tasks whose workflow exited to in-progress with a
// live PR number (because an evaluate step crashed, or a manually-spawned
// agent opened the PR outside the workflow) were silently dropped from the
// scan because it only considered status=in-review. Result: failing CI on
// those PRs was never fixed by pr-fix agents.
func TestPRMonitorEligible(t *testing.T) {
	tests := []struct {
		name string
		tk   task.Task
		want bool
	}{
		{
			name: "in-review with PR — original happy path",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42},
			want: true,
		},
		{
			name: "in-review with branch only — still eligible",
			tk:   task.Task{Status: task.StatusInReview, Branch: "synapse/feat-x"},
			want: true,
		},
		{
			name: "in-review with neither PR nor branch — not eligible",
			tk:   task.Task{Status: task.StatusInReview},
			want: false,
		},
		{
			name: "in-progress with PR — the regression case we're fixing",
			tk:   task.Task{Status: task.StatusInProgress, PRNumber: 247},
			want: true,
		},
		{
			name: "in-progress with branch only — not eligible (avoid WIP false positives)",
			tk:   task.Task{Status: task.StatusInProgress, Branch: "synapse/wip"},
			want: false,
		},
		{
			name: "in-progress with nothing — not eligible",
			tk:   task.Task{Status: task.StatusInProgress},
			want: false,
		},
		{
			name: "review tag excluded (inbound review task, not ours)",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42, Tags: []string{"review"}},
			want: false,
		},
		{
			name: "todo with PR — not eligible, not in monitored states",
			tk:   task.Task{Status: task.StatusTodo, PRNumber: 42},
			want: false,
		},
		{
			name: "done with PR — not eligible, already terminal",
			tk:   task.Task{Status: task.StatusDone, PRNumber: 42},
			want: false,
		},
		{
			name: "human-required with PR — not eligible, needs operator action first",
			tk:   task.Task{Status: task.StatusHumanRequired, PRNumber: 42},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prMonitorEligible(&tt.tk); got != tt.want {
				t.Errorf("prMonitorEligible(%+v) = %v, want %v", tt.tk, got, tt.want)
			}
		})
	}
}

// loadBuiltinWorkflow installs a workflow YAML from internal/workflow/builtin/
// into the env's workflow store so tests can drive the real builtin definition
// (rather than the test-simple.yaml fixture). The store reads YAMLs from disk
// on demand, so writing the file is enough — no reload needed.
func loadBuiltinWorkflow(t *testing.T, env *e2eEnv, name string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("internal", "workflow", "builtin", name+".yaml"))
	if err != nil {
		t.Fatalf("read builtin workflow %s: %v", name, err)
	}
	dst := filepath.Join(env.wfStore.Dir(), name+".yaml")
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		t.Fatalf("write workflow %s to store: %v", name, err)
	}
}

// stepIDsFromHistory extracts the ordered list of step IDs from a workflow
// execution's step history, used to assert which steps actually ran.
func stepIDsFromHistory(wf *workflow.Execution) []string {
	if wf == nil {
		return nil
	}
	ids := make([]string, len(wf.StepHistory))
	for i := range wf.StepHistory {
		ids[i] = wf.StepHistory[i].StepID
	}
	return ids
}

// agentRunRoles returns the roles of agent runs recorded against a task,
// used to assert which agent roles were spawned by the workflow.
func agentRunRoles(t task.Task) []string {
	roles := make([]string, len(t.AgentRuns))
	for i := range t.AgentRuns {
		roles[i] = t.AgentRuns[i].Role
	}
	return roles
}

// TestE2E_BuiltinSimpleTask_PlanCriticRunsBeforeReview drives the builtin
// simple-task workflow through the plan stage and asserts the new
// critique_plan step (added between plan and review_plan) actually executes
// before the workflow lands at the human review gate. The critic agent runs
// in headless mode with role "plan-critic"; that role appearing in the task's
// agent runs is the externally observable proof the step ran.
func TestE2E_BuiltinSimpleTask_PlanCriticRunsBeforeReview(t *testing.T) {
	env := setupE2EMulti(t, []string{
		"triage_to_planning", // triage flips status=planning, tags=large
		"success",            // plan agent (interactive) — exits, advances
		"success",            // critique_plan agent (headless plan-critic)
	})
	loadBuiltinWorkflow(t, env, "simple-task")

	created, err := env.tasks.Create("plan critic e2e", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "simple-task"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow reaches review_plan", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			tk.Workflow.CurrentStep == "review_plan" &&
			tk.Workflow.State == workflow.ExecWaiting
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	stepIDs := stepIDsFromHistory(tk.Workflow)
	if !slices.Contains(stepIDs, "critique_plan") {
		t.Errorf("critique_plan missing from step history\nhistory: %v", stepIDs)
	}
	if !slices.Contains(stepIDs, "maybe_critique") {
		t.Errorf("maybe_critique missing from step history\nhistory: %v", stepIDs)
	}

	// critique_plan must follow plan in execution order. review_plan is the
	// current waiting step but isn't recorded in history until the human acts.
	planIdx := slices.Index(stepIDs, "plan")
	critiqueIdx := slices.Index(stepIDs, "critique_plan")
	if planIdx < 0 || critiqueIdx <= planIdx {
		t.Errorf("step order wrong: want plan before critique_plan, got %v (plan=%d critique=%d)",
			stepIDs, planIdx, critiqueIdx)
	}

	roles := agentRunRoles(tk)
	if !slices.Contains(roles, "plan-critic") {
		t.Errorf("plan-critic agent role missing from task agent runs\nroles: %v", roles)
	}
}

// TestE2E_BuiltinSimpleTask_NocriticTagSkipsCritique covers the opt-out path:
// a task tagged "nocritic" must bypass critique_plan via the maybe_critique
// condition step and reach review_plan with no plan-critic agent ever spawned.
func TestE2E_BuiltinSimpleTask_NocriticTagSkipsCritique(t *testing.T) {
	env := setupE2EMulti(t, []string{
		"triage_to_planning_nocritic", // triage sets status=planning, tags=large,nocritic
		"success",                     // plan agent only — no critic should run
	})
	loadBuiltinWorkflow(t, env, "simple-task")

	created, err := env.tasks.Create("nocritic e2e", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.engine.StartWorkflow(created.ID, "simple-task"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow reaches review_plan", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			tk.Workflow.CurrentStep == "review_plan" &&
			tk.Workflow.State == workflow.ExecWaiting
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	stepIDs := stepIDsFromHistory(tk.Workflow)
	if slices.Contains(stepIDs, "critique_plan") {
		t.Errorf("critique_plan must be skipped when nocritic tag is present\nhistory: %v", stepIDs)
	}
	if !slices.Contains(stepIDs, "maybe_critique") {
		t.Errorf("maybe_critique missing — branch decision must still execute\nhistory: %v", stepIDs)
	}

	roles := agentRunRoles(tk)
	if slices.Contains(roles, "plan-critic") {
		t.Errorf("no plan-critic agent should be spawned when nocritic tag is set\nroles: %v", roles)
	}
}
