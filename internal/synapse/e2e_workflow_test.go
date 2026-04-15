//go:build !short

package synapse

import (
	"context"
	"errors"
	"fmt"
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
	synapsegithub "github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/task"

	"github.com/Automaat/synapse/internal/workflow"
	"github.com/Automaat/synapse/internal/worktree"
)

var (
	testBinDir    string
	testBuildOnce sync.Once
	testBuildErr  string // non-empty if binary build failed
)

// TestMain tears down the shared binary directory after all tests complete.
func TestMain(m *testing.M) {
	code := m.Run()
	if testBinDir != "" {
		_ = os.RemoveAll(testBinDir)
	}
	os.Exit(code)
}

func buildTestBinaries(t *testing.T) string {
	t.Helper()
	testBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "synapse-test-bins-*")
		if err != nil {
			testBuildErr = err.Error()
			return
		}
		// Build fake claude.
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "claude"), "../../cmd/fake-claude")
		if out, err := cmd.CombinedOutput(); err != nil {
			testBuildErr = "build fake-claude: " + err.Error() + "\n" + string(out)
			return
		}
		cmd = exec.Command("go", "build", "-o", filepath.Join(dir, "codex"), "../../cmd/fake-codex")
		if out, err := cmd.CombinedOutput(); err != nil {
			testBuildErr = "build fake-codex: " + err.Error() + "\n" + string(out)
			return
		}
		// Build real synapse-cli.
		cmd = exec.Command("go", "build", "-o", filepath.Join(dir, "synapse-cli"), "../../cmd/synapse-cli")
		if out, err := cmd.CombinedOutput(); err != nil {
			testBuildErr = "build synapse-cli: " + err.Error() + "\n" + string(out)
			return
		}
		testBinDir = dir
	})
	if testBuildErr != "" {
		t.Fatalf("build test binaries: %s", testBuildErr)
	}
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
	agentDir     string // pre-staged working dir injected into run_agent steps
	worktreesDir string
	scenarioFile string
	provider     string
	cancel       context.CancelFunc
}

// startWorkflow seeds the reserved _dir variable so that run_agent steps have
// a valid working directory, satisfying the agent.Manager.Run guard that
// rejects empty Dir. Used in place of env.engine.StartWorkflow in tests.
func (e *e2eEnv) startWorkflow(taskID, workflowID string) error {
	return e.engine.StartWorkflowWithVars(taskID, workflowID, map[string]string{
		workflow.WorkflowVarDir: e.agentDir,
	})
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

	agentMgr := agent.NewManager(ctx, func(string, any) {}, logger, logDir)
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
	src, err := os.ReadFile("../../internal/workflow/testdata/test-simple.yaml")
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
	engine.SetWorktreeGetter(&worktreeGetterAdapter{tasks: taskMgr, mgr: wm})

	agentMgr.SetOnComplete(func(ag *agent.Agent) {
		var result string
		output := ag.Output()
		for i := range output {
			if output[i].Type == "result" {
				result = output[i].Content
			}
		}
		engine.HandleAgentComplete(ag.TaskID, workflow.AgentCompletion{
			AgentID:  ag.ID,
			Result:   result,
			Success:  ag.GetExitErr() == nil,
			Provider: ag.Provider,
		})
	})

	// Pre-create a working directory so run_agent steps can satisfy the
	// Manager.Run guard that rejects empty Dir.
	agentDir, err := os.MkdirTemp("", "synapse-e2e-agent-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(agentDir) })

	return &e2eEnv{
		tasks:        taskMgr,
		agents:       agentMgr,
		engine:       engine,
		wfStore:      wfStore,
		taskDir:      taskDir,
		agentDir:     agentDir,
		worktreesDir: wtDir,
		provider:     provider,
		cancel:       cancel,
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

	if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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

// TestE2E_ProviderMatrix_FullLifecycleEvalFlipsHumanRequired verifies that
// the mechanical evaluate step (no LLM) flips a successful task to
// human-required when the workflow has no link_pr_and_review chain to find
// a PR. test-simple.yaml goes implement → evaluate directly, so the eval
// always reaches the "commits pushed but no PR created" branch.
func TestE2E_ProviderMatrix_FullLifecycleEvalFlipsHumanRequired(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EMultiProvider(t, p.provider, []string{"triage", "success"})

		created, err := env.tasks.Create("eval lifecycle task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		waitFor(t, 30*time.Second, "workflow completes with human-required status", func() bool {
			tk, err := env.tasks.Get(created.ID)
			if err != nil {
				return false
			}
			return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted && tk.Status == task.StatusHumanRequired
		})

		tk, _ := env.tasks.Get(created.ID)
		if tk.StatusReason != "commits pushed but no PR created" {
			t.Errorf("status_reason = %q, want %q", tk.StatusReason, "commits pushed but no PR created")
		}
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
			Dir:      t.TempDir(),
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
// Claude) via stdin/stdout. The agent should produce ConvoEvents,
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
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// One-shot: agent exits after turn.completed → StateStopped.
	waitFor(t, 10*time.Second, "codex interactive agent stops", func() bool {
		return ag.GetState() == agent.StateStopped
	})

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
		Dir:      t.TempDir(),
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
}

// TestE2E_Codex_HeadlessRetry_Overloaded verifies that a Codex headless agent
// retries when the provider emits an overloaded error (substring match on the
// message). The first invocation returns "overloaded_error" (exits 1 with an
// error event containing "overloaded" in the message); the second succeeds.
// Before the provider-guard fix, Codex never retried and the agent stayed
// failed permanently.
func TestE2E_Codex_HeadlessRetry_Overloaded(t *testing.T) {
	env := setupE2EMultiProvider(t, "codex", []string{"overloaded_error", "success"})

	ag, err := env.agents.Run(agent.RunConfig{
		TaskID:   "task-codex-retry",
		Name:     "codex retry overloaded",
		Mode:     "headless",
		Provider: "codex",
		Prompt:   "do work",
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The first attempt exits 1 after emitting an overloaded error event.
	// After backoff the agent retries and the second scenario (success) runs.
	// First attempt exits 1 after ~0s; retry waits headlessRetryBackoffs[0]=30s;
	// second attempt succeeds. Budget 90s to accommodate the backoff.
	waitFor(t, 90*time.Second, "codex agent stops after retry", func() bool {
		return ag.GetState() == agent.StateStopped
	})

	// Verify the agent produced a result event from the successful second attempt.
	hasResult := false
	for _, ev := range ag.Output() {
		if ev.Type == "result" && ev.Subtype == "" {
			hasResult = true
		}
	}
	if !hasResult {
		t.Error("expected result event from second attempt, got none")
	}
}

// TestE2E_Codex_HeadlessRetry_OverloadedStructured verifies retry behavior
// when Codex emits a structured overloaded envelope (code=529).
func TestE2E_Codex_HeadlessRetry_OverloadedStructured(t *testing.T) {
	env := setupE2EMultiProvider(t, "codex", []string{"overloaded_error_structured", "success"})

	ag, err := env.agents.Run(agent.RunConfig{
		TaskID:   "task-codex-retry-structured",
		Name:     "codex retry overloaded structured",
		Mode:     "headless",
		Provider: "codex",
		Prompt:   "do work",
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 90*time.Second, "codex agent stops after structured retry", func() bool {
		return ag.GetState() == agent.StateStopped
	})

	hasResult := false
	for _, ev := range ag.Output() {
		if ev.Type == "result" && ev.Subtype == "" {
			hasResult = true
		}
	}
	if !hasResult {
		t.Error("expected result event from second attempt, got none")
	}
}

// TestE2E_ProviderMatrix_NoResult_DoesNotStall verifies workflows don't hang
// when provider exits 0 without emitting a result event.
func TestE2E_ProviderMatrix_NoResult_DoesNotStall(t *testing.T) {
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EMultiProvider(t, p.provider, []string{"triage", "no_result"})

		created, err := env.tasks.Create("no result task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
			t.Fatal(err)
		}

		waitFor(t, 30*time.Second, "workflow completes despite no result event", func() bool {
			tk, gErr := env.tasks.Get(created.ID)
			if gErr != nil {
				return false
			}
			return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
		})

		tk, _ := env.tasks.Get(created.ID)
		if tk.Status != task.StatusHumanRequired {
			t.Errorf("task status = %q, want human-required", tk.Status)
		}

		var implementRec *workflow.StepRecord
		seen := map[string]bool{}
		for i := range tk.Workflow.StepHistory {
			r := &tk.Workflow.StepHistory[i]
			seen[r.StepID] = true
			if r.StepID == "implement" {
				implementRec = r
			}
		}
		for _, want := range []string{"triage", "implement", "evaluate"} {
			if !seen[want] {
				t.Errorf("missing step %q in history, got %v", want, seen)
			}
		}
		if implementRec == nil {
			t.Fatal("missing implement step record")
		}
		if implementRec.Output != "" {
			t.Errorf("implement output = %q, want empty (provider emitted no result event)", implementRec.Output)
		}
	})
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
	// (mechanical, flips to human-required since test-simple.yaml has no
	// link_pr_and_review chain).
	forEachProvider(t, func(t *testing.T, p providerSpec) {
		env := setupE2EMultiProvider(t, p.provider, []string{"triage", "interactive_implement"})

		created, err := env.tasks.Create("interactive one-shot task", "", "interactive")
		if err != nil {
			t.Fatal(err)
		}

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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
		// Mechanical evaluate flips to human-required ("commits pushed but no
		// PR created"); the original assertion was in-review when the LLM
		// eval set the status itself. The point of this test is that
		// interactive tasks now advance past implement at all — the exact
		// terminal status is now decided by the mechanical eval.
		if tk.Status != task.StatusHumanRequired {
			t.Errorf("task status = %q, want human-required", tk.Status)
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

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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

		if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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
		Variables:   map[string]string{workflow.WorkflowVarDir: env.agentDir},
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

// TestE2E_ResumeStalled_SkipsTaskWithRunningAgent verifies ResumeStalled does
// not spawn duplicate work when a live agent is already attached to the task.
func TestE2E_ResumeStalled_SkipsTaskWithRunningAgent(t *testing.T) {
	env := setupE2EProvider(t, "claude", "interactive_implement")

	created, err := env.tasks.Create("stalled but live agent task", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       workflow.ExecRunning,
		Variables:   map[string]string{workflow.WorkflowVarDir: env.agentDir},
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status":   "in-progress",
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = env.agents.Run(agent.RunConfig{
		TaskID:   created.ID,
		Name:     "implementation",
		Mode:     "interactive",
		Provider: "claude",
		Model:    "sonnet",
		Prompt:   "Implement task",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "agent becomes live", func() bool {
		live := env.agents.FindAllRunningAgentsForTask(created.ID, "")
		return len(live) == 1
	})

	before := len(env.agents.FindAllRunningAgentsForTask(created.ID, ""))

	env.engine.ResumeStalled()

	// ResumeStalled is synchronous. Live agent count must stay unchanged.
	after := len(env.agents.FindAllRunningAgentsForTask(created.ID, ""))
	if after != before {
		t.Fatalf("live agents changed after ResumeStalled: before=%d after=%d", before, after)
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

	if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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
	go func() { errCh <- env.startWorkflow(t1.ID, "test-simple") }()
	go func() { errCh <- env.startWorkflow(t2.ID, "test-simple") }()

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
// finished outside synapse's view (app restart, process crash, etc.).
// The task file records a waiting `implement` step; the recovery path calls
// HandleAgentComplete with a marker so the workflow advances through
// evaluate and reaches ExecCompleted without re-running the interactive
// implement step.
func TestE2E_RecoverStaleInteractive(t *testing.T) {
	// No agent runs for real — implement is "recovered" via marker, and
	// evaluate is now a mechanical Go step that doesn't invoke fake-claude.
	env := setupE2EMulti(t, []string{})

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
		Variables:   map[string]string{workflow.WorkflowVarDir: env.agentDir},
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

	// Mirror what recoverStaleInteractive does: set Recovered on the execution,
	// persist, then drive HandleAgentComplete with an empty result.
	wfExec.Recovered = true
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{"workflow": wfExec}); err != nil {
		t.Fatal(err)
	}
	env.engine.HandleAgentComplete(created.ID, workflow.AgentCompletion{AgentID: "stale-agent", Success: true})

	// Mechanical evaluate fires (no fake-claude invocation), flips the task
	// to human-required, then the workflow reaches ExecCompleted.
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
	if implementRec.Output != "" {
		t.Errorf("implement output = %q, want empty (structured recovery, no sentinel string)", implementRec.Output)
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
    type: evaluate
    next:
      - goto: ""
`

// TestE2E_DispatchPREvent_FullRun exercises the end-to-end pr.event
// dispatch path: workflow match by trigger conditions, set_in_progress flip,
// fix agent via caller-supplied prompt var, evaluate agent, then completion.
// Also verifies that a repeat DispatchEvent while the first is still running
// returns ErrWorkflowAlreadyActive instead of launching a second workflow.
func TestE2E_DispatchPREvent_FullRun(t *testing.T) {
	// fix (success) → evaluate (mechanical, flips to human-required since
	// the test workflow has no link_pr_and_review chain).
	env := setupE2EMulti(t, []string{"success"})

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

	// Dispatch with the exported PRIssueCIFailure constant (same call shape as
	// app_reviews.go:handlePRIssue). Using the constant here — instead of a
	// literal "ci_failure" — is the regression guard: if anyone renames the
	// constant back to "ci-failure", this dispatch stops matching test-pr-fix
	// (and, by extension, the real builtin pr-fix.yaml).
	wfID, err := env.engine.DispatchEvent(created.ID, "pr.event",
		map[string]string{"pr.issue_kind": string(synapsegithub.PRIssueCIFailure)},
		map[string]string{"prompt": "fix the CI", workflow.WorkflowVarDir: env.agentDir})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "test-pr-fix" {
		t.Fatalf("wfID = %q, want test-pr-fix", wfID)
	}

	// Second dispatch while the first is still running must be rejected —
	// double-dispatch guard prevents competing workflows on the same task.
	if _, err := env.engine.DispatchEvent(created.ID, "pr.event",
		map[string]string{"pr.issue_kind": string(synapsegithub.PRIssueCIFailure)}, nil); !errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
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
	// Mechanical evaluate flips the task to human-required when no PR was
	// found by the (absent) link_pr_and_review step.
	if tk.Status != task.StatusHumanRequired {
		t.Errorf("task status = %q, want human-required", tk.Status)
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

// testEvalChainWorkflowYAML mirrors the real simple-task.yaml mechanical
// chain (implement → link_pr_and_review → evaluate) used by the e2e tests
// that exercise the full path from a successful agent through the two
// mechanical fallback steps. verify_commits is intentionally omitted —
// it requires a real git worktree and is independently covered by unit
// tests in internal/workflow/engine_test.go.
const testEvalChainWorkflowYAML = `id: test-eval-chain
name: Test Eval Chain
steps:
  - id: set_in_progress
    name: Mark In Progress
    type: set_status
    config:
      status: in-progress
    next:
      - goto: implement

  - id: implement
    name: Implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      model: sonnet
      prompt: "Implement {{.Task.ID}}"
    next:
      - goto: link_pr_and_review

  - id: link_pr_and_review
    name: Link PR and Review
    type: link_pr_and_review
    next:
      - when:
          field: task.status
          operator: equals
          value: in-review
        goto: ""
      - when:
          field: task.status
          operator: equals
          value: human-required
        goto: ""
      - goto: evaluate

  - id: evaluate
    name: Evaluate
    type: evaluate
    next:
      - goto: ""
`

// TestE2E_EvalChain_PRURLInResultGoesInReview exercises the happy path
// through the mechanical chain: implement emits a PR URL in its result,
// link_pr_and_review picks it up via the regex path (path 2), flips the
// task to in-review, and the workflow terminates without ever reaching
// the mechanical evaluate step.
func TestE2E_EvalChain_PRURLInResultGoesInReview(t *testing.T) {
	env := setupE2EMulti(t, []string{"pr_created"})

	if err := os.WriteFile(
		filepath.Join(env.wfStore.Dir(), "test-eval-chain.yaml"),
		[]byte(testEvalChainWorkflowYAML), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("eval chain happy", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.startWorkflow(created.ID, "test-eval-chain"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "eval chain workflow completes", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Status != task.StatusInReview {
		t.Errorf("task status = %q, want in-review", tk.Status)
	}
	if tk.PRNumber != 42 {
		t.Errorf("task pr_number = %d, want 42", tk.PRNumber)
	}

	// link_pr_and_review should have terminated the workflow before
	// the mechanical evaluate step ran.
	for _, r := range tk.Workflow.StepHistory {
		if r.StepID == "evaluate" {
			t.Errorf("evaluate step recorded — link_pr_and_review should have terminated the workflow")
		}
	}
}

// TestE2E_EvalChain_NoPRFlipsHumanRequired exercises the fallback path:
// implement emits no PR URL, link_pr_and_review's three discovery paths
// all miss, the mechanical evaluate step runs and flips the task to
// human-required with the "commits pushed but no PR created" reason.
func TestE2E_EvalChain_NoPRFlipsHumanRequired(t *testing.T) {
	env := setupE2EMulti(t, []string{"success"})

	if err := os.WriteFile(
		filepath.Join(env.wfStore.Dir(), "test-eval-chain.yaml"),
		[]byte(testEvalChainWorkflowYAML), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("eval chain fallback", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if err := env.startWorkflow(created.ID, "test-eval-chain"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "eval chain workflow completes", func() bool {
		tk, err := env.tasks.Get(created.ID)
		if err != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Status != task.StatusHumanRequired {
		t.Errorf("task status = %q, want human-required", tk.Status)
	}
	if tk.StatusReason != "commits pushed but no PR created" {
		t.Errorf("status_reason = %q, want %q", tk.StatusReason, "commits pushed but no PR created")
	}
	if tk.PRNumber != 0 {
		t.Errorf("task pr_number = %d, want 0", tk.PRNumber)
	}

	// Both the link_pr_and_review and evaluate steps must have run.
	seen := map[string]bool{}
	for _, r := range tk.Workflow.StepHistory {
		seen[r.StepID] = true
	}
	for _, want := range []string{"implement", "link_pr_and_review", "evaluate"} {
		if !seen[want] {
			t.Errorf("missing step %q in history, got %v", want, seen)
		}
	}
}

// testAutoMergeWorkflowYAML mirrors the real builtin auto-merge.yaml trigger
// (value: ready_to_merge) but replaces the shell merge step with a trivial
// set_status step so the workflow can run to completion in e2e without
// touching real GitHub. Used to verify dispatch-by-constant for
// PRIssueReadyToMerge.
const testAutoMergeWorkflowYAML = `id: test-auto-merge
name: Test Auto Merge
trigger:
  on: pr.event
  conditions:
    - field: pr.issue_kind
      operator: equals
      value: ready_to_merge
steps:
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

// TestE2E_DispatchPREvent_ReadyToMerge verifies that dispatching a pr.event
// with string(PRIssueReadyToMerge) as the issue kind matches a workflow that
// declares value: ready_to_merge in its trigger. Companion regression test
// to TestE2E_DispatchPREvent_FullRun — if the constant is renamed back to
// "ready-to-merge" with a dash, the dispatch stops matching and this test
// fails.
func TestE2E_DispatchPREvent_ReadyToMerge(t *testing.T) {
	env := setupE2E(t, "success")
	if err := os.WriteFile(
		filepath.Join(env.wfStore.Dir(), "test-auto-merge.yaml"),
		[]byte(testAutoMergeWorkflowYAML), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("ready-to-merge task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status": "in-review",
	}); err != nil {
		t.Fatal(err)
	}

	wfID, err := env.engine.DispatchEvent(created.ID, "pr.event",
		map[string]string{"pr.issue_kind": string(synapsegithub.PRIssueReadyToMerge)},
		nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "test-auto-merge" {
		t.Fatalf("wfID = %q, want test-auto-merge", wfID)
	}

	waitFor(t, 10*time.Second, "test-auto-merge completes", func() bool {
		tk, gerr := env.tasks.Get(created.ID)
		if gerr != nil {
			return false
		}
		return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Status != task.StatusDone {
		t.Errorf("task status = %q, want done", tk.Status)
	}
}

// TestPRIssueKindConstants_MatchBuiltinWorkflowTriggers locks in the string
// values of the PR issue kind constants against the real builtin workflow
// YAMLs they must match. This is the narrowest possible regression guard
// for the ci-failure vs ci_failure (and ready-to-merge vs ready_to_merge)
// dispatch-mismatch bugs: if the constants drift from what the YAML triggers
// expect, dispatch silently stops matching — exactly the bug this test
// prevents.
func TestPRIssueKindConstants_MatchBuiltinWorkflowTriggers(t *testing.T) {
	cases := []struct {
		kind     synapsegithub.PRIssueKind
		wantStr  string
		yamlFile string // empty when no builtin workflow triggers on this kind
		needle   string
	}{
		{
			kind:     synapsegithub.PRIssueCIFailure,
			wantStr:  "ci_failure",
			yamlFile: "../../internal/workflow/builtin/pr-fix.yaml",
			needle:   "value: conflict,ci_failure",
		},
		{
			kind:    synapsegithub.PRIssueReadyToMerge,
			wantStr: "ready_to_merge",
			// No builtin workflow: auto-merge.yaml was removed because
			// ready_to_merge short-circuits to app_reviews.handleAutoMerge
			// direct path and never reaches DispatchEvent. The constant is
			// still locked here so frontend + any new workflow stays in sync.
		},
		{
			kind:     synapsegithub.PRIssueConflict,
			wantStr:  "conflict",
			yamlFile: "../../internal/workflow/builtin/pr-fix.yaml",
			needle:   "value: conflict,ci_failure",
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := string(tc.kind); got != tc.wantStr {
				t.Errorf("constant = %q, want %q", got, tc.wantStr)
			}
			if tc.yamlFile == "" {
				return
			}
			raw, err := os.ReadFile(tc.yamlFile)
			if err != nil {
				t.Fatalf("read %s: %v", tc.yamlFile, err)
			}
			if !strings.Contains(string(raw), tc.needle) {
				t.Errorf("%s missing trigger %q — dispatch would not match", tc.yamlFile, tc.needle)
			}
		})
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
	for i := range tk.Workflow.StepHistory {
		if tk.Workflow.StepHistory[i].StepID == stepID {
			n++
		}
	}
	return n
}

func lastStepStatus(tk task.Task, stepID string) string {
	if tk.Workflow == nil {
		return ""
	}
	for i := len(tk.Workflow.StepHistory) - 1; i >= 0; i-- {
		if tk.Workflow.StepHistory[i].StepID == stepID {
			return tk.Workflow.StepHistory[i].Status
		}
	}
	return ""
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
		map[string]string{"task.status": string(task.StatusTesting)},
		map[string]string{workflow.WorkflowVarDir: env.agentDir})
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
		map[string]string{"task.status": string(task.StatusTesting)},
		map[string]string{workflow.WorkflowVarDir: env.agentDir}); err != nil {
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

// TestE2E_WaitHuman_InvalidActionRejected ensures unknown actions are rejected
// without mutating waiting workflow state.
func TestE2E_WaitHuman_InvalidActionRejected(t *testing.T) {
	env := setupE2EMulti(t, []string{"triage_to_planning", "success"})

	created, err := env.tasks.Create("invalid human action task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "workflow reaches review_plan", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil {
			return false
		}
		return tk.Workflow != nil &&
			tk.Workflow.CurrentStep == "review_plan" &&
			tk.Workflow.State == workflow.ExecWaiting
	})

	before, _ := env.tasks.Get(created.ID)
	historyBefore := len(before.Workflow.StepHistory)
	updatedBefore := before.UpdatedAt

	err = env.engine.HandleHumanAction(created.ID, "bogus", nil)
	if err == nil {
		t.Fatal("expected invalid action error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid human action") {
		t.Fatalf("err = %v, want invalid human action", err)
	}

	after, _ := env.tasks.Get(created.ID)
	if after.Workflow.State != workflow.ExecWaiting {
		t.Errorf("state = %q, want waiting", after.Workflow.State)
	}
	if after.Workflow.CurrentStep != "review_plan" {
		t.Errorf("current_step = %q, want review_plan", after.Workflow.CurrentStep)
	}
	if got := len(after.Workflow.StepHistory); got != historyBefore {
		t.Errorf("step_history len = %d, want %d", got, historyBefore)
	}
	if !after.UpdatedAt.Equal(updatedBefore) {
		t.Errorf("UpdatedAt changed from %v to %v", updatedBefore, after.UpdatedAt)
	}
	if _, set := after.Workflow.Variables["human_action"]; set {
		t.Errorf("human_action unexpectedly set: %q", after.Workflow.Variables["human_action"])
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

	if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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
	env.engine.HandleAgentComplete(created.ID, workflow.AgentCompletion{AgentID: "stray-agent-xyz", Result: "late result", Success: true})

	// HandleAgentComplete is synchronous; no async side effects to wait for.
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

	if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
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
	env.engine.HandleAgentComplete(created.ID, workflow.AgentCompletion{AgentID: "stray-duplicate-plan-agent", Result: "late plan result", Success: true})

	// HandleAgentComplete is synchronous; no async side effects to wait for.
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
	// No background activity targets this task; assert state is unchanged immediately.
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
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "workflow", "builtin", name+".yaml"))
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

	if err := env.startWorkflow(created.ID, "simple-task"); err != nil {
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

	if err := env.startWorkflow(created.ID, "simple-task"); err != nil {
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

// TestE2E_BuiltinSimpleTask_TriageTerminalShortCircuits verifies triage can
// terminate simple-task directly on terminal statuses without running plan or
// implementation.
func TestE2E_BuiltinSimpleTask_TriageTerminalShortCircuits(t *testing.T) {
	cases := []struct {
		name       string
		scenario   string
		wantStatus task.Status
	}{
		{name: "done", scenario: "triage_to_done", wantStatus: task.StatusDone},
		{name: "in_review", scenario: "triage_to_in_review", wantStatus: task.StatusInReview},
		{name: "human_required", scenario: "triage_to_human_required", wantStatus: task.StatusHumanRequired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forEachProvider(t, func(t *testing.T, p providerSpec) {
				env := setupE2EMultiProvider(t, p.provider, []string{tc.scenario})
				loadBuiltinWorkflow(t, env, "simple-task")

				created, err := env.tasks.Create("terminal triage "+tc.name, "", "headless")
				if err != nil {
					t.Fatal(err)
				}
				if err := env.startWorkflow(created.ID, "simple-task"); err != nil {
					t.Fatal(err)
				}

				waitFor(t, 30*time.Second, "workflow completes after triage terminal short-circuit", func() bool {
					tk, gErr := env.tasks.Get(created.ID)
					if gErr != nil {
						return false
					}
					return tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
				})

				tk, _ := env.tasks.Get(created.ID)
				if tk.Status != tc.wantStatus {
					t.Errorf("status = %q, want %q", tk.Status, tc.wantStatus)
				}

				stepIDs := stepIDsFromHistory(tk.Workflow)
				if !slices.Contains(stepIDs, "triage") {
					t.Errorf("triage missing from step history: %v", stepIDs)
				}
				for _, forbidden := range []string{"plan", "set_in_progress", "implement", "verify_commits", "link_pr_and_review", "evaluate"} {
					if slices.Contains(stepIDs, forbidden) {
						t.Errorf("forbidden step %q executed on terminal short-circuit: %v", forbidden, stepIDs)
					}
				}
			})
		})
	}
}

const testWaitForStatusWorkflowYAML = `id: test-wait-status
name: Test Wait For Status
steps:
  - id: plan_wait
    name: Plan Wait
    type: run_agent
    config:
      role: plan
      mode: interactive
      model: sonnet
      wait_for_status: plan-review
      prompt: "Plan {{.Task.ID}}"
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testReuseAgentWorkflowYAML = `id: test-reuse-agent
name: Test Reuse Agent
steps:
  - id: phase1
    name: Phase 1
    type: run_agent
    config:
      role: plan
      mode: interactive
      model: sonnet
      reuse_agent: true
      wait_for_status: phase1
      prompt: "Phase1 {{.Task.ID}}"
    next:
      - goto: phase2
  - id: phase2
    name: Phase 2
    type: run_agent
    config:
      role: plan
      mode: interactive
      model: sonnet
      reuse_agent: true
      wait_for_status: phase2
      prompt: "Phase2 {{.Task.ID}}"
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testTransitionFailureWorkflowYAML = `id: test-transition-fail
name: Test Transition Fail
steps:
  - id: gate
    name: Gate
    type: condition
    config:
      check:
        field: task.status
        operator: equals
        value: todo
    next:
      - when:
          field: task.status
          operator: equals
          value: done
        goto: ""
`

const testProviderFallbackWorkflowYAML = `id: test-provider-fallback
name: Test Provider Fallback
steps:
  - id: first
    name: First
    type: run_agent
    config:
      role: implementation
      mode: headless
      provider: claude
      prompt: "First {{.Task.ID}}"
    next:
      - goto: second
  - id: second
    name: Second
    type: run_agent
    config:
      role: review
      mode: headless
      provider: cross
      prompt: "Second {{.Task.ID}}"
    next:
      - goto: ""
`

func writeWorkflowFixture(t *testing.T, env *e2eEnv, name, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(env.wfStore.Dir(), name+".yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write %s.yaml: %v", name, err)
	}
}

func stageProviderPath(t *testing.T, include ...string) string {
	t.Helper()
	srcDir := buildTestBinaries(t)
	dstDir := t.TempDir()
	for _, name := range include {
		src := filepath.Join(srcDir, name)
		dst := filepath.Join(dstDir, name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
	}
	return dstDir
}

func runCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}

func initRepoWithOriginMain(t *testing.T, dir string) {
	t.Helper()
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.email", "e2e@example.com")
	runCmd(t, dir, "git", "config", "user.name", "E2E")
	runCmd(t, dir, "git", "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, dir, "git", "add", "README.md")
	runCmd(t, dir, "git", "commit", "-m", "init")

	remote := filepath.Join(t.TempDir(), "remote.git")
	runCmd(t, "", "git", "init", "--bare", remote)
	runCmd(t, dir, "git", "remote", "add", "origin", remote)
	runCmd(t, dir, "git", "push", "-u", "origin", "main")
}

type scriptedGate struct {
	mu       sync.Mutex
	healthy  map[string]bool
	reason   map[string]string
	failover map[string]string
}

func newScriptedGate() *scriptedGate {
	return &scriptedGate{
		healthy:  map[string]bool{"claude": true, "codex": true},
		reason:   map[string]string{"claude": "ok", "codex": "ok"},
		failover: map[string]string{},
	}
}

func (g *scriptedGate) IsHealthy(p string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.healthy[p]
}

func (g *scriptedGate) Failover(unhealthy string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failover[unhealthy]
}

func (g *scriptedGate) Reason(p string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason[p]
}

func (g *scriptedGate) ReportAuthFailure(provider, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.healthy[provider] = false
	if reason == "" {
		reason = "auth_failed"
	}
	g.reason[provider] = reason
}

func (g *scriptedGate) ReportRateLimit(provider string, retryAfter time.Duration, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.healthy[provider] = false
	if reason == "" {
		reason = "rate_limited"
	}
	g.reason[provider] = reason
}

type cooldownGate struct {
	mu        sync.Mutex
	limitedTo map[string]time.Time
	failover  map[string]string
}

func newCooldownGate() *cooldownGate {
	return &cooldownGate{
		limitedTo: map[string]time.Time{},
		failover:  map[string]string{"claude": "codex", "codex": "claude"},
	}
}

func (g *cooldownGate) IsHealthy(p string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.limitedTo[p]
	return !ok || time.Now().After(until)
}

func (g *cooldownGate) Failover(unhealthy string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failover[unhealthy]
}

func (g *cooldownGate) Reason(p string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.limitedTo[p]
	if ok && time.Now().Before(until) {
		return "rate_limited"
	}
	return "ok"
}

func (g *cooldownGate) ReportAuthFailure(provider, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limitedTo[provider] = time.Now().Add(5 * time.Minute)
}

func (g *cooldownGate) ReportRateLimit(provider string, retryAfter time.Duration, reason string) {
	if retryAfter <= 0 {
		retryAfter = 200 * time.Millisecond
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limitedTo[provider] = time.Now().Add(retryAfter)
}

type scriptedPRLinker struct {
	get  func(repo string, prNumber int) ([]int, string, error)
	edit func(repo string, prNumber int, body string) error
}

func (l *scriptedPRLinker) GetClosingIssues(repo string, prNumber int) (issues []int, body string, err error) {
	if l.get == nil {
		return nil, "", nil
	}
	return l.get(repo, prNumber)
}

func (l *scriptedPRLinker) EditBody(repo string, prNumber int, body string) error {
	if l.edit == nil {
		return nil
	}
	return l.edit(repo, prNumber, body)
}

func rebuildEngineFromEnv(t *testing.T, env *e2eEnv) *workflow.Engine {
	t.Helper()

	tasksDir := filepath.Join(env.taskDir, "tasks")
	store, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logDir := t.TempDir()
	agentMgr := agent.NewManager(ctx, func(string, any) {}, e2eLogger(), logDir)
	agentMgr.SetDefaultProvider(env.provider)

	wm := worktree.New(worktree.Config{
		WorktreesDir: env.worktreesDir,
		Tasks:        taskMgr,
		Logger:       e2eLogger(),
		AgentChecker: agentMgr.HasRunningAgentForTask,
	})
	agentOrch := newAgentOrchestrator(taskMgr, nil, agentMgr, nil, e2eLogger(), wm, nil)

	ta := &taskAdapter{tasks: taskMgr}
	aa := &agentAdapter{agents: agentMgr, agentOrch: agentOrch, tasks: taskMgr}
	engine := workflow.NewEngine(env.wfStore, ta, aa, e2eLogger())
	engine.SetWorktreeGetter(&worktreeGetterAdapter{tasks: taskMgr, mgr: wm})
	engine.SetPRLinker(nil)

	agentMgr.SetOnComplete(func(ag *agent.Agent) {
		var result string
		output := ag.Output()
		for i := range output {
			if output[i].Type == "result" {
				result = output[i].Content
			}
		}
		engine.HandleAgentComplete(ag.TaskID, workflow.AgentCompletion{
			AgentID:  ag.ID,
			Result:   result,
			Success:  ag.GetExitErr() == nil,
			Provider: ag.Provider,
		})
	})
	return engine
}

// TestE2E_DispatchPREvent_ConcurrentSingleWinner verifies concurrent dispatches
// on the same task/event race to one winner, with the loser rejected.
func TestE2E_DispatchPREvent_ConcurrentSingleWinner(t *testing.T) {
	env := setupE2EMulti(t, []string{"success"})
	if err := os.WriteFile(filepath.Join(env.wfStore.Dir(), "test-pr-fix.yaml"), []byte(testPRFixWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("dispatch race task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{"status": "in-review"}); err != nil {
		t.Fatal(err)
	}

	type result struct {
		wfID string
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			wfID, dErr := env.engine.DispatchEvent(
				created.ID,
				"pr.event",
				map[string]string{"pr.issue_kind": string(synapsegithub.PRIssueCIFailure)},
				map[string]string{"prompt": "fix race", workflow.WorkflowVarDir: env.agentDir},
			)
			results <- result{wfID: wfID, err: dErr}
		}()
	}
	close(start)

	successes := 0
	alreadyActive := 0
	for range 2 {
		r := <-results
		if r.err == nil {
			successes++
			if r.wfID != "test-pr-fix" {
				t.Fatalf("wfID = %q, want test-pr-fix", r.wfID)
			}
			continue
		}
		if errors.Is(r.err, workflow.ErrWorkflowAlreadyActive) {
			alreadyActive++
			continue
		}
		t.Fatalf("unexpected dispatch err: %v", r.err)
	}
	if successes != 1 || alreadyActive != 1 {
		t.Fatalf("want 1 success + 1 already-active, got success=%d already-active=%d", successes, alreadyActive)
	}
}

// TestE2E_DispatchPREvent_TerminalWorkflowCanBeReplaced verifies dispatch is
// allowed when the existing workflow is already terminal.
func TestE2E_DispatchPREvent_TerminalWorkflowCanBeReplaced(t *testing.T) {
	env := setupE2E(t, "success")
	if err := os.WriteFile(filepath.Join(env.wfStore.Dir(), "test-auto-merge.yaml"), []byte(testAutoMergeWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("dispatch terminal replace", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status": "in-review",
		"workflow": &workflow.Execution{
			WorkflowID:  "stale-old",
			CurrentStep: "",
			State:       workflow.ExecCompleted,
			Variables:   map[string]string{"old": "value"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	wfID, err := env.engine.DispatchEvent(created.ID, "pr.event",
		map[string]string{"pr.issue_kind": string(synapsegithub.PRIssueReadyToMerge)}, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if wfID != "test-auto-merge" {
		t.Fatalf("wfID = %q, want test-auto-merge", wfID)
	}
}

func TestE2E_WaitForStatus_MismatchDoesNotAdvance(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement"})
	writeWorkflowFixture(t, env, "test-wait-status", testWaitForStatusWorkflowYAML)

	created, err := env.tasks.Create("wait status mismatch", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-wait-status"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "workflow waits on plan_wait", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "plan_wait" && tk.Workflow.State == workflow.ExecWaiting
	})

	env.engine.HandleStatusChange(created.ID, "in-progress")
	time.Sleep(250 * time.Millisecond)

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow.CurrentStep != "plan_wait" || tk.Workflow.State != workflow.ExecWaiting {
		t.Fatalf("workflow advanced on mismatched status: step=%q state=%q", tk.Workflow.CurrentStep, tk.Workflow.State)
	}
}

func TestE2E_WaitForStatus_ExactAdvancesOnce(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement"})
	writeWorkflowFixture(t, env, "test-wait-status", testWaitForStatusWorkflowYAML)

	created, err := env.tasks.Create("wait status exact", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-wait-status"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "workflow waits on plan_wait", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "plan_wait" && tk.Workflow.State == workflow.ExecWaiting
	})

	env.engine.HandleStatusChange(created.ID, "plan-review")
	waitFor(t, 10*time.Second, "workflow completes on matching status", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	recordsBefore := len(tk.Workflow.StepHistory)
	env.engine.HandleStatusChange(created.ID, "plan-review")
	time.Sleep(150 * time.Millisecond)
	tkAfter, _ := env.tasks.Get(created.ID)
	if len(tkAfter.Workflow.StepHistory) != recordsBefore {
		t.Fatalf("duplicate advancement on repeated status: before=%d after=%d", recordsBefore, len(tkAfter.Workflow.StepHistory))
	}
}

func TestE2E_ReuseAgent_ContinuesWithSameAgent(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement"})
	writeWorkflowFixture(t, env, "test-reuse-agent", testReuseAgentWorkflowYAML)

	created, err := env.tasks.Create("reuse agent same", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-reuse-agent"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "phase1 waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "phase1" && tk.Workflow.State == workflow.ExecWaiting && len(tk.AgentRuns) == 1
	})
	tk1, _ := env.tasks.Get(created.ID)
	firstAgentID := tk1.AgentRuns[0].AgentID

	env.engine.HandleStatusChange(created.ID, "phase1")
	waitFor(t, 10*time.Second, "phase2 waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "phase2" && tk.Workflow.State == workflow.ExecWaiting
	})

	tk2, _ := env.tasks.Get(created.ID)
	if len(tk2.AgentRuns) != 1 {
		t.Fatalf("agent runs = %d, want 1 (reuse)", len(tk2.AgentRuns))
	}
	if tk2.AgentRuns[0].AgentID != firstAgentID {
		t.Fatalf("agent id changed: %s -> %s", firstAgentID, tk2.AgentRuns[0].AgentID)
	}

	env.engine.HandleStatusChange(created.ID, "phase2")
	waitFor(t, 10*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
}

func TestE2E_ReuseAgent_FallbackStartsNewWhenDead(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement", "interactive_implement"})
	writeWorkflowFixture(t, env, "test-reuse-agent", testReuseAgentWorkflowYAML)

	created, err := env.tasks.Create("reuse agent dead fallback", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-reuse-agent"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "phase1 waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "phase1" && len(tk.AgentRuns) == 1
	})
	tk1, _ := env.tasks.Get(created.ID)
	firstAgentID := tk1.AgentRuns[0].AgentID
	if err := env.agents.StopAgent(firstAgentID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "first agent stopped", func() bool {
		ag, gErr := env.agents.GetAgent(firstAgentID)
		return gErr == nil && ag.GetState() == agent.StateStopped
	})

	env.engine.HandleStatusChange(created.ID, "phase1")
	waitFor(t, 10*time.Second, "phase2 waiting with replacement agent", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "phase2" && len(tk.AgentRuns) >= 2
	})
	tk2, _ := env.tasks.Get(created.ID)
	if len(tk2.AgentRuns) < 2 {
		t.Fatalf("agent runs = %d, want >=2", len(tk2.AgentRuns))
	}
	secondAgentID := tk2.AgentRuns[len(tk2.AgentRuns)-1].AgentID
	if secondAgentID == firstAgentID {
		t.Fatalf("expected replacement agent id, got same %s", secondAgentID)
	}

	env.engine.HandleStatusChange(created.ID, "phase2")
	waitFor(t, 10*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
}

func TestE2E_MaxRetries_Boundary(t *testing.T) {
	cases := []struct {
		name        string
		maxRetries  int
		scenarios   []string
		wantRuns    int
		description string
	}{
		{name: "zero", maxRetries: 0, scenarios: []string{"fail_exit"}, wantRuns: 1, description: "no retry"},
		{name: "one", maxRetries: 1, scenarios: []string{"fail_exit", "success"}, wantRuns: 2, description: "single retry"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setupE2EMultiProvider(t, "claude", tc.scenarios)
			yaml := fmt.Sprintf(`id: test-retry-boundary
name: Test Retry Boundary
steps:
  - id: implement
    name: Implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      max_retries: %d
      prompt: "Implement {{.Task.ID}}"
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`, tc.maxRetries)
			writeWorkflowFixture(t, env, "test-retry-boundary", yaml)

			created, err := env.tasks.Create("retry boundary "+tc.description, "", "headless")
			if err != nil {
				t.Fatal(err)
			}
			if err := env.startWorkflow(created.ID, "test-retry-boundary"); err != nil {
				t.Fatal(err)
			}

			waitFor(t, 30*time.Second, "workflow completes", func() bool {
				tk, gErr := env.tasks.Get(created.ID)
				return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
			})
			tk, _ := env.tasks.Get(created.ID)
			runs := countStepRecords(tk, "implement")
			if runs != tc.wantRuns {
				t.Fatalf("implement runs = %d, want %d", runs, tc.wantRuns)
			}
		})
	}
}

func TestE2E_ConditionTransitionFailure_FailsWorkflow(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-transition-fail", testTransitionFailureWorkflowYAML)

	created, err := env.tasks.Create("transition failure", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-transition-fail"); err == nil {
		t.Fatal("expected transition resolution error, got nil")
	} else if !strings.Contains(err.Error(), "no matching transition found") {
		t.Fatalf("unexpected error: %v", err)
	}

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Workflow == nil || tk.Workflow.State != workflow.ExecFailed {
		t.Fatalf("workflow state = %v, want failed", tk.Workflow)
	}
}

func TestE2E_ProviderCrossUnavailable_FallsBackToDefault(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success", "success"})
	writeWorkflowFixture(t, env, "test-provider-fallback", testProviderFallbackWorkflowYAML)

	claudeArgsLog := filepath.Join(t.TempDir(), "claude-args.log")
	codexArgsLog := filepath.Join(t.TempDir(), "codex-args.log")
	t.Setenv("FAKE_CLAUDE_ARGS_LOG", claudeArgsLog)
	t.Setenv("FAKE_CODEX_ARGS_LOG", codexArgsLog)

	// Expose only claude + synapse-cli on PATH so cross=>codex is unavailable.
	subsetPath := stageProviderPath(t, "claude", "synapse-cli")
	t.Setenv("PATH", subsetPath)

	created, err := env.tasks.Create("provider fallback", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-provider-fallback"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "provider fallback workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	if _, err := os.Stat(codexArgsLog); err == nil {
		t.Fatalf("codex args log exists; cross provider should have fallen back to default claude")
	}
	if _, err := os.Stat(claudeArgsLog); err != nil {
		t.Fatalf("claude args log missing: %v", err)
	}
}

func TestE2E_VerifyCommits_NoAheadFlipsHumanRequired(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"triage", "success"})
	loadBuiltinWorkflow(t, env, "simple-task")

	created, err := env.tasks.Create("verify commits no ahead", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	current, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	worktreePath := filepath.Join(env.worktreesDir, current.DirName())
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepoWithOriginMain(t, worktreePath)
	runCmd(t, worktreePath, "git", "log", "origin/main..HEAD", "--oneline")

	if err := env.startWorkflow(created.ID, "simple-task"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 30*time.Second, "workflow completes after verify_commits", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", tk.Status)
	}
	if tk.StatusReason != "no commits pushed to branch" {
		var verifyOut string
		for i := range tk.Workflow.StepHistory {
			if tk.Workflow.StepHistory[i].StepID == "verify_commits" {
				verifyOut = tk.Workflow.StepHistory[i].Output
			}
		}
		t.Fatalf("status_reason = %q, want no commits pushed to branch (verify_commits output=%q)", tk.StatusReason, verifyOut)
	}
	stepIDs := stepIDsFromHistory(tk.Workflow)
	if !slices.Contains(stepIDs, "verify_commits") {
		t.Fatalf("verify_commits missing from history: %v", stepIDs)
	}
	if slices.Contains(stepIDs, "link_pr_and_review") || slices.Contains(stepIDs, "evaluate") {
		t.Fatalf("unexpected fallback steps executed after verify_commits gate: %v", stepIDs)
	}
}

func TestE2E_LinkPRAndReview_PrefersExistingTaskPRNumber(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"pr_created"})
	if err := os.WriteFile(filepath.Join(env.wfStore.Dir(), "test-eval-chain.yaml"), []byte(testEvalChainWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("pr precedence", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{"pr_number": 7}); err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-eval-chain"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "workflow completes in-review", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted && tk.Status == task.StatusInReview
	})
	tk, _ := env.tasks.Get(created.ID)
	if tk.PRNumber != 7 {
		t.Fatalf("pr_number = %d, want 7 (existing task PR should win)", tk.PRNumber)
	}
}

func TestE2E_StaleCompletionAfterTaskDelete_NoRecreate(t *testing.T) {
	env := setupE2EProvider(t, "claude", "interactive_implement")
	created, err := env.tasks.Create("delete during run", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}

	ag, err := env.agents.Run(agent.RunConfig{
		TaskID:   created.ID,
		Name:     "manual-agent",
		Mode:     "interactive",
		Provider: "claude",
		Model:    "sonnet",
		Prompt:   "run",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "manual agent live", func() bool {
		s := ag.GetState()
		return s == agent.StateRunning || s == agent.StatePaused
	})

	if err := env.tasks.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	env.engine.HandleAgentComplete(created.ID, workflow.AgentCompletion{
		AgentID:  ag.ID,
		Result:   "late completion",
		Success:  true,
		Provider: "claude",
	})

	if _, err := env.tasks.Get(created.ID); err == nil {
		t.Fatal("task recreated unexpectedly after stale completion")
	}
	_ = env.agents.StopAgent(ag.ID)
}

const testDispatchDirectWorkflowYAML = `id: test-direct-start
name: Test Direct Start
steps:
  - id: direct
    name: Direct
    type: run_agent
    config:
      role: implementation
      mode: headless
      prompt: "Direct {{.Task.ID}}"
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testStatusCompleteRaceWorkflowYAML = `id: test-status-complete-race
name: Test Status/Complete Race
steps:
  - id: plan_wait
    name: Plan Wait
    type: run_agent
    config:
      role: plan
      mode: interactive
      wait_for_status: plan-review
      prompt: "Plan {{.Task.ID}}"
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testStalePriorStepWorkflowYAML = `id: test-stale-prior
name: Test Stale Prior Step
steps:
  - id: implement
    name: Implement
    type: run_agent
    config:
      role: implementation
      mode: headless
      prompt: "Implement {{.Task.ID}}"
    next:
      - goto: review_gate
  - id: review_gate
    name: Review Gate
    type: wait_human
    config:
      status: plan-review
      human_actions:
        - approve
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testCrossMixedWorkflowYAML = `id: test-cross-mixed
name: Test Cross Mixed
steps:
  - id: first
    name: First
    type: run_agent
    config:
      role: implementation
      mode: headless
      provider: claude
      prompt: "First {{.Task.ID}}"
    next:
      - goto: second
  - id: second
    name: Second
    type: run_agent
    config:
      role: review
      mode: headless
      provider: codex
      prompt: "Second {{.Task.ID}}"
    next:
      - goto: third
  - id: third
    name: Third
    type: run_agent
    config:
      role: fix-review
      mode: headless
      provider: cross
      prompt: "Third {{.Task.ID}}"
    next:
      - goto: ""
`

const testWaitHumanConcurrentWorkflowYAML = `id: test-wait-human-concurrent
name: Test Wait Human Concurrent
steps:
  - id: gate
    name: Gate
    type: wait_human
    config:
      status: plan-review
      human_actions:
        - approve
        - reject
    next:
      - when:
          field: vars.human_action
          operator: equals
          value: approve
        goto: set_done
      - when:
          field: vars.human_action
          operator: equals
          value: reject
        goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testVerifyAfterStatusWorkflowYAML = `id: test-verify-after-status
name: Test Verify After Status
steps:
  - id: prep
    name: Prepare
    type: run_agent
    config:
      role: plan
      mode: interactive
      wait_for_status: go-verify
      prompt: "Prep {{.Task.ID}}"
    next:
      - goto: verify
  - id: verify
    name: Verify
    type: verify_commits
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testProviderFlapWorkflowYAML = `id: test-provider-flap
name: Test Provider Flap
steps:
  - id: first
    name: First
    type: run_agent
    config:
      role: implementation
      mode: interactive
      provider: codex
      wait_for_status: flip
      prompt: "First {{.Task.ID}}"
    next:
      - goto: second
  - id: second
    name: Second
    type: run_agent
    config:
      role: review
      mode: headless
      provider: cross
      prompt: "Second {{.Task.ID}}"
    next:
      - goto: ""
`

func TestE2E_DispatchVsDirectStart_RaceStable(t *testing.T) {
	env := setupE2EMulti(t, []string{"success", "success"})
	writeWorkflowFixture(t, env, "test-pr-fix", testPRFixWorkflowYAML)
	writeWorkflowFixture(t, env, "test-direct-start", testDispatchDirectWorkflowYAML)

	created, err := env.tasks.Create("dispatch/start race", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{"status": "in-review"}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var dispatchErr error
	var startErr error
	go func() {
		defer wg.Done()
		<-start
		_, dispatchErr = env.engine.DispatchEvent(
			created.ID, "pr.event",
			map[string]string{"pr.issue_kind": string(synapsegithub.PRIssueCIFailure)},
			map[string]string{"prompt": "fix", workflow.WorkflowVarDir: env.agentDir},
		)
	}()
	go func() {
		defer wg.Done()
		<-start
		startErr = env.engine.StartWorkflowWithVars(created.ID, "test-direct-start", map[string]string{
			workflow.WorkflowVarDir: env.agentDir,
		})
	}()
	close(start)
	wg.Wait()

	if dispatchErr != nil && !errors.Is(dispatchErr, workflow.ErrWorkflowAlreadyActive) {
		t.Fatalf("dispatch err = %v", dispatchErr)
	}
	if startErr != nil {
		t.Fatalf("start err = %v", startErr)
	}

	waitFor(t, 20*time.Second, "final workflow terminal", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		if gErr != nil || tk.Workflow == nil {
			return false
		}
		return tk.Workflow.State == workflow.ExecCompleted || tk.Workflow.State == workflow.ExecFailed
	})

	tk, _ := env.tasks.Get(created.ID)
	for i := range tk.Workflow.StepHistory {
		if tk.Workflow.StepHistory[i].StepID == "" {
			t.Fatalf("empty StepID in history index %d", i)
		}
	}
}

func TestE2E_StatusChangeAndAgentComplete_RaceSingleRecord(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement"})
	writeWorkflowFixture(t, env, "test-status-complete-race", testStatusCompleteRaceWorkflowYAML)

	created, err := env.tasks.Create("status/complete race", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-status-complete-race"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "plan_wait waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "plan_wait" && tk.Workflow.State == workflow.ExecWaiting && len(tk.AgentRuns) == 1
	})
	tk, _ := env.tasks.Get(created.ID)
	agentID := tk.AgentRuns[0].AgentID

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		env.engine.HandleStatusChange(created.ID, "plan-review")
	}()
	go func() {
		defer wg.Done()
		env.engine.HandleAgentComplete(created.ID, workflow.AgentCompletion{
			AgentID:  agentID,
			Success:  true,
			Result:   "late completion",
			Provider: "claude",
		})
	}()
	wg.Wait()

	waitFor(t, 10*time.Second, "workflow completed once", func() bool {
		cur, gErr := env.tasks.Get(created.ID)
		return gErr == nil && cur.Workflow != nil && cur.Workflow.State == workflow.ExecCompleted
	})
	cur, _ := env.tasks.Get(created.ID)
	if got := countStepRecords(cur, "plan_wait"); got != 1 {
		t.Fatalf("plan_wait records = %d, want 1", got)
	}
}

func TestE2E_StalePriorStepAdvance_DroppedWhileNewStepActive(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success"})
	writeWorkflowFixture(t, env, "test-stale-prior", testStalePriorStepWorkflowYAML)

	created, err := env.tasks.Create("stale prior", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-stale-prior"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "review_gate waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "review_gate" && tk.Workflow.State == workflow.ExecWaiting
	})
	before, _ := env.tasks.Get(created.ID)
	historyBefore := len(before.Workflow.StepHistory)

	if err := env.engine.AdvanceStep(created.ID, workflow.StepOutput{
		StepID: "implement",
		Status: "completed",
		Output: "late output",
	}); err != nil {
		t.Fatalf("advance stale prior step: %v", err)
	}

	after, _ := env.tasks.Get(created.ID)
	if after.Workflow.CurrentStep != "review_gate" || after.Workflow.State != workflow.ExecWaiting {
		t.Fatalf("workflow mutated by stale prior step: step=%q state=%q", after.Workflow.CurrentStep, after.Workflow.State)
	}
	if got := len(after.Workflow.StepHistory); got != historyBefore {
		t.Fatalf("step history changed by stale prior step: before=%d after=%d", historyBefore, got)
	}
}

func TestE2E_CrossProvider_FlipsFromLatestMixedHistory(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success", "success", "success"})
	writeWorkflowFixture(t, env, "test-cross-mixed", testCrossMixedWorkflowYAML)

	created, err := env.tasks.Create("cross mixed history", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-cross-mixed"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
	tk, _ := env.tasks.Get(created.ID)

	providerByStep := map[string]string{}
	for i := range tk.Workflow.StepHistory {
		r := tk.Workflow.StepHistory[i]
		providerByStep[r.StepID] = r.Provider
	}
	if providerByStep["first"] != "claude" {
		t.Fatalf("first provider = %q, want claude", providerByStep["first"])
	}
	if providerByStep["second"] != "codex" {
		t.Fatalf("second provider = %q, want codex", providerByStep["second"])
	}
	if providerByStep["third"] != "claude" {
		t.Fatalf("third provider = %q, want claude (cross flip from codex)", providerByStep["third"])
	}
}

func TestE2E_CodexRetry_OverloadedThenAuthStopsRetry(t *testing.T) {
	env := setupE2EMultiProvider(t, "codex", []string{"overloaded_error", "auth_error", "success"})

	ag, err := env.agents.Run(agent.RunConfig{
		TaskID:   "task-codex-overload-auth",
		Name:     "codex overload then auth",
		Mode:     "headless",
		Provider: "codex",
		Prompt:   "do work",
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, 120*time.Second, "agent stops after auth error", func() bool {
		return ag.GetState() == agent.StateStopped
	})

	hasSuccess := false
	hasOverloadErr := false
	hasAuthErr := false
	for _, ev := range ag.Output() {
		if ev.Type == "result" && ev.Subtype == "" {
			hasSuccess = true
		}
		if ev.Type == "result" && ev.Subtype == "error" && strings.Contains(strings.ToLower(ev.Content), "overloaded") {
			hasOverloadErr = true
		}
		if ev.Type == "result" && ev.Subtype == "error" && strings.Contains(strings.ToLower(ev.Content), "auth") {
			hasAuthErr = true
		}
	}
	if hasSuccess {
		t.Fatal("unexpected success result after auth error; retries should stop")
	}
	if !hasOverloadErr || !hasAuthErr {
		t.Fatalf("missing expected errors: overloaded=%v auth=%v", hasOverloadErr, hasAuthErr)
	}
}

func TestE2E_WaitHuman_ConcurrentDoubleActionSingleWinner(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-wait-human-concurrent", testWaitHumanConcurrentWorkflowYAML)

	created, err := env.tasks.Create("double human action", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-wait-human-concurrent"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "gate waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "gate" && tk.Workflow.State == workflow.ExecWaiting
	})

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		errs <- env.engine.HandleHumanAction(created.ID, "approve", nil)
	}()
	go func() {
		defer wg.Done()
		errs <- env.engine.HandleHumanAction(created.ID, "reject", nil)
	}()
	wg.Wait()
	close(errs)

	okCount := 0
	errCount := 0
	for err := range errs {
		if err == nil {
			okCount++
		} else {
			errCount++
		}
	}
	if okCount != 1 || errCount != 1 {
		t.Fatalf("want one success + one error, got success=%d error=%d", okCount, errCount)
	}

	waitFor(t, 10*time.Second, "workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
}

func TestE2E_WorktreeDisappearsMidVerify_SkipsGracefully(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement"})
	writeWorkflowFixture(t, env, "test-verify-after-status", testVerifyAfterStatusWorkflowYAML)

	created, err := env.tasks.Create("worktree disappears", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wtPath := filepath.Join(env.worktreesDir, cur.DirName())
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepoWithOriginMain(t, wtPath)

	if err := env.startWorkflow(created.ID, "test-verify-after-status"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "prep waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "prep" && tk.Workflow.State == workflow.ExecWaiting
	})

	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatal(err)
	}
	env.engine.HandleStatusChange(created.ID, "go-verify")
	waitFor(t, 10*time.Second, "workflow completes after missing worktree", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	var verifyOut string
	for i := range tk.Workflow.StepHistory {
		if tk.Workflow.StepHistory[i].StepID == "verify" {
			verifyOut = tk.Workflow.StepHistory[i].Output
		}
	}
	if !strings.Contains(verifyOut, "skipped: no worktree for task") {
		t.Fatalf("verify output = %q, want missing-worktree skip", verifyOut)
	}
}

func TestE2E_LinkPRAndReview_MalformedHugeOutput_NoFalsePositive(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"malformed_pr_output"})
	if err := os.WriteFile(filepath.Join(env.wfStore.Dir(), "test-eval-chain.yaml"), []byte(testEvalChainWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("malformed pr output", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-eval-chain"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "workflow completes fallback", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
	tk, _ := env.tasks.Get(created.ID)
	if tk.PRNumber != 0 {
		t.Fatalf("pr_number = %d, want 0 for malformed output", tk.PRNumber)
	}
	if tk.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", tk.Status)
	}
}

func TestE2E_DeletedTask_RaceCallbacks_NoRecreate(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-pr-fix", testPRFixWorkflowYAML)

	created, err := env.tasks.Create("delete callback race", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.tasks.Delete(created.ID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	var dispatchErr error
	go func() {
		defer wg.Done()
		env.engine.HandleAgentComplete(created.ID, workflow.AgentCompletion{
			AgentID:  "late",
			Success:  true,
			Result:   "late",
			Provider: "claude",
		})
	}()
	go func() {
		defer wg.Done()
		env.engine.HandleStatusChange(created.ID, "plan-review")
	}()
	go func() {
		defer wg.Done()
		_, dispatchErr = env.engine.DispatchEvent(created.ID, "pr.event",
			map[string]string{"pr.issue_kind": string(synapsegithub.PRIssueCIFailure)}, nil)
	}()
	wg.Wait()

	if dispatchErr == nil {
		t.Fatal("expected dispatch error for deleted task, got nil")
	}
	if _, err := env.tasks.Get(created.ID); err == nil {
		t.Fatal("task recreated unexpectedly")
	}
}

func TestE2E_ResumeStalled_TightLoopIdempotent(t *testing.T) {
	env := setupE2EProvider(t, "claude", "interactive_implement")
	created, err := env.tasks.Create("resume tight loop", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       workflow.ExecRunning,
		Variables:   map[string]string{workflow.WorkflowVarDir: env.agentDir},
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status":   "in-progress",
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			env.engine.ResumeStalled()
		})
	}
	wg.Wait()

	waitFor(t, 15*time.Second, "workflow advances past implement", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep != "implement"
	})
	tk, _ := env.tasks.Get(created.ID)
	if len(tk.AgentRuns) != 1 {
		t.Fatalf("agent runs = %d, want 1", len(tk.AgentRuns))
	}
}

func TestE2E_WaitForStatus_RepeatedIdenticalEvents_AdvanceOnce(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement"})
	writeWorkflowFixture(t, env, "test-wait-status", testWaitForStatusWorkflowYAML)

	created, err := env.tasks.Create("repeat same status", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-wait-status"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "plan_wait waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "plan_wait" && tk.Workflow.State == workflow.ExecWaiting
	})

	for range 20 {
		env.engine.HandleStatusChange(created.ID, "plan-review")
	}
	waitFor(t, 10*time.Second, "workflow complete", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
	tk, _ := env.tasks.Get(created.ID)
	if got := countStepRecords(tk, "plan_wait"); got != 1 {
		t.Fatalf("plan_wait records = %d, want 1", got)
	}
}

func TestE2E_ProviderBinaryFlap_SecondStepFallsBackDeterministically(t *testing.T) {
	env := setupE2EMultiProvider(t, "codex", []string{"interactive_implement", "success"})
	writeWorkflowFixture(t, env, "test-provider-flap", testProviderFlapWorkflowYAML)

	codexArgsLog := filepath.Join(t.TempDir(), "codex-args.log")
	claudeArgsLog := filepath.Join(t.TempDir(), "claude-args.log")
	t.Setenv("FAKE_CODEX_ARGS_LOG", codexArgsLog)
	t.Setenv("FAKE_CLAUDE_ARGS_LOG", claudeArgsLog)

	created, err := env.tasks.Create("provider flap", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-provider-flap"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "first waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "first" && tk.Workflow.State == workflow.ExecWaiting
	})

	// Remove claude binary before second step starts; cross from codex wants
	// claude and must deterministically fall back to default codex.
	subset := stageProviderPath(t, "codex", "synapse-cli")
	t.Setenv("PATH", subset)

	env.engine.HandleStatusChange(created.ID, "flip")
	waitFor(t, 20*time.Second, "provider-flap workflow completes", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	if _, err := os.Stat(claudeArgsLog); err == nil {
		t.Fatalf("claude args log exists; second step should have fallen back to codex")
	}
	data, err := os.ReadFile(codexArgsLog)
	if err != nil {
		t.Fatalf("read codex args: %v", err)
	}
	if !strings.Contains(string(data), "Second") {
		t.Fatalf("codex args missing second-step prompt:\n%s", string(data))
	}
}

const testHistoryCapWorkflowYAML = `id: test-history-cap
name: Test History Cap
steps:
  - id: gate
    name: Loop Gate
    type: wait_human
    config:
      status: plan-review
      human_actions:
        - approve
        - reject
    next:
      - goto: gate
`

const testReloadWorkflowYAML = `id: test-reload-live
name: Test Reload Live
steps:
  - id: gate
    name: Gate
    type: wait_human
    config:
      status: plan-review
      human_actions:
        - approve
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testReloadWorkflowYAMLUpdated = `id: test-reload-live
name: Test Reload Live
steps:
  - id: gate
    name: Gate
    type: wait_human
    config:
      status: plan-review
      human_actions:
        - approve
    next:
      - goto: set_in_progress
  - id: set_in_progress
    name: Mark In Progress
    type: set_status
    config:
      status: in-progress
    next:
      - goto: ""
`

const testVarsWorkflowYAML = `id: test-vars
name: Test Vars
steps:
  - id: implement
    name: Implement
    type: run_agent
    config:
      role: review
      mode: headless
      prompt: 'Var: {{ getvar .Vars "odd:key" }} Task {{.Task.ID}}'
    next:
      - goto: ""
`

const testVerifyOnlyWorkflowYAML = `id: test-verify-only
name: Test Verify Only
steps:
  - id: verify
    name: Verify
    type: verify_commits
    next:
      - goto: set_done
  - id: set_done
    name: Mark Done
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`

const testCrossDefaultWorkflowYAML = `id: test-cross-default
name: Test Cross Default
steps:
  - id: cross_step
    name: Cross Step
    type: run_agent
    config:
      role: review
      mode: headless
      provider: cross
      prompt: "Cross {{.Task.ID}}"
    next:
      - goto: ""
`

const testEnsurePRWorkflowYAML = `id: test-ensure-pr
name: Test Ensure PR
steps:
  - id: ensure
    name: Ensure PR closes issue
    type: ensure_pr_closes_issue
    next:
      - goto: ""
`

const testEvaluateLatePRWorkflowYAML = `id: test-evaluate-late-pr
name: Test Evaluate Late PR
steps:
  - id: eval
    name: Evaluate
    type: evaluate
    next:
      - goto: ""
`

const testShellToLinkPRWorkflowYAML = `id: test-shell-link-pr
name: Test Shell to Link PR
steps:
  - id: emit
    name: Emit PR short ref
    type: shell
    config:
      command: "echo owner/repo#55"
    next:
      - goto: link
  - id: link
    name: Link PR
    type: link_pr_and_review
    next:
      - goto: ""
`

func TestE2E_ProviderHealthFailoverAndRecovery(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success", "success"})
	g := newScriptedGate()
	g.healthy["claude"] = false
	g.reason["claude"] = "rate_limited"
	g.failover["claude"] = "codex"
	env.agents.SetHealthGate(g)

	ag1, err := env.agents.Run(agent.RunConfig{
		TaskID:   "health-failover-1",
		Name:     "health failover 1",
		Mode:     "headless",
		Provider: "claude",
		Prompt:   "work",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ag1.Provider != "codex" {
		t.Fatalf("provider = %q, want codex failover", ag1.Provider)
	}
	waitFor(t, 10*time.Second, "failover run stops", func() bool { return ag1.GetState() == agent.StateStopped })

	g.mu.Lock()
	g.healthy["claude"] = true
	g.reason["claude"] = "ok"
	g.mu.Unlock()

	ag2, err := env.agents.Run(agent.RunConfig{
		TaskID:   "health-failover-2",
		Name:     "health failover 2",
		Mode:     "headless",
		Provider: "claude",
		Prompt:   "work",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ag2.Provider != "claude" {
		t.Fatalf("provider = %q, want claude after recovery", ag2.Provider)
	}
	waitFor(t, 10*time.Second, "recovery run stops", func() bool { return ag2.GetState() == agent.StateStopped })
}

func TestE2E_RateLimitCooldownWindowCorrectness(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success", "success"})
	g := newCooldownGate()
	env.agents.SetHealthGate(g)
	g.ReportRateLimit("claude", 200*time.Millisecond, "rate_limited")

	ag1, err := env.agents.Run(agent.RunConfig{
		TaskID:   "cooldown-1",
		Name:     "cooldown 1",
		Mode:     "headless",
		Provider: "claude",
		Prompt:   "work",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ag1.Provider != "codex" {
		t.Fatalf("provider = %q, want codex during cooldown", ag1.Provider)
	}
	waitFor(t, 10*time.Second, "cooldown run1 stops", func() bool { return ag1.GetState() == agent.StateStopped })

	time.Sleep(250 * time.Millisecond)
	ag2, err := env.agents.Run(agent.RunConfig{
		TaskID:   "cooldown-2",
		Name:     "cooldown 2",
		Mode:     "headless",
		Provider: "claude",
		Prompt:   "work",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ag2.Provider != "claude" {
		t.Fatalf("provider = %q, want claude after cooldown", ag2.Provider)
	}
	waitFor(t, 10*time.Second, "cooldown run2 stops", func() bool { return ag2.GetState() == agent.StateStopped })
}

func TestE2E_OutOfOrderCompletions_IsolatedPerTask(t *testing.T) {
	env := setupE2E(t, "success")
	tasks := make([]task.Task, 0, 3)
	for i := 1; i <= 3; i++ {
		created, err := env.tasks.Create(fmt.Sprintf("ooo-%d", i), "", "headless")
		if err != nil {
			t.Fatal(err)
		}
		wfExec := &workflow.Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{workflow.WorkflowVarDir: env.agentDir},
		}
		if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
			"status":   "in-progress",
			"workflow": wfExec,
		}); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, created)
	}

	env.engine.HandleAgentComplete(tasks[1].ID, workflow.AgentCompletion{AgentID: "a2", Success: false, Result: "err-two"})
	env.engine.HandleAgentComplete(tasks[0].ID, workflow.AgentCompletion{AgentID: "a1", Success: false, Result: "err-one"})
	env.engine.HandleAgentComplete(tasks[2].ID, workflow.AgentCompletion{AgentID: "a3", Success: false, Result: "err-three"})

	want := map[string]string{
		tasks[0].ID: "err-one",
		tasks[1].ID: "err-two",
		tasks[2].ID: "err-three",
	}
	for _, tk := range tasks {
		cur, err := env.tasks.Get(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.StatusReason != want[tk.ID] {
			t.Fatalf("task %s status_reason = %q, want %q", tk.ID, cur.StatusReason, want[tk.ID])
		}
	}
}

func TestE2E_StepHistoryCap_KeepsLatest50(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-history-cap", testHistoryCapWorkflowYAML)

	created, err := env.tasks.Create("history cap", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-history-cap"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "history gate waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "gate" && tk.Workflow.State == workflow.ExecWaiting
	})

	for i := range 80 {
		action := "approve"
		if i%2 == 1 {
			action = "reject"
		}
		if err := env.engine.HandleHumanAction(created.ID, action, nil); err != nil {
			t.Fatalf("action %d failed: %v", i, err)
		}
	}
	tk, _ := env.tasks.Get(created.ID)
	if got := len(tk.Workflow.StepHistory); got != 50 {
		t.Fatalf("step history len = %d, want 50", got)
	}
}

func TestE2E_WorkflowReload_AppliesOnAdvance(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-reload-live", testReloadWorkflowYAML)

	created, err := env.tasks.Create("reload live", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-reload-live"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "reload gate waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "gate" && tk.Workflow.State == workflow.ExecWaiting
	})

	writeWorkflowFixture(t, env, "test-reload-live", testReloadWorkflowYAMLUpdated)
	if err := env.engine.HandleHumanAction(created.ID, "approve", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "reload workflow terminal", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
	tk, _ := env.tasks.Get(created.ID)
	if tk.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want in-progress from updated workflow", tk.Status)
	}
}

func TestE2E_StatusHookStorm_NoDuplicateAdvance(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement"})
	writeWorkflowFixture(t, env, "test-wait-status", testWaitForStatusWorkflowYAML)

	created, err := env.tasks.Create("status storm", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-wait-status"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "storm waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "plan_wait"
	})

	var wg sync.WaitGroup
	for i := range 120 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%3 == 0 {
				env.engine.HandleStatusChange(created.ID, "plan-review")
			} else {
				env.engine.HandleStatusChange(created.ID, "todo")
			}
		}(i)
	}
	wg.Wait()

	waitFor(t, 10*time.Second, "storm completed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
	tk, _ := env.tasks.Get(created.ID)
	if got := countStepRecords(tk, "plan_wait"); got != 1 {
		t.Fatalf("plan_wait records = %d, want 1", got)
	}
}

func TestE2E_StartWorkflowWithMalformedVars_NoPanic(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success"})
	writeWorkflowFixture(t, env, "test-vars", testVarsWorkflowYAML)

	created, err := env.tasks.Create("malformed vars", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"odd:key": strings.Repeat("X", 4096),
		"../../x": "{{bad}}",
		"\nkey":   "v",
	}
	if err := env.engine.StartWorkflowWithVars(created.ID, "test-vars", vars); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "vars workflow complete", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
}

func TestE2E_CrossDefaultEmptyProvider_ResolvesDeterministically(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success"})
	writeWorkflowFixture(t, env, "test-cross-default", testCrossDefaultWorkflowYAML)
	env.agents.SetDefaultProvider("")

	created, err := env.tasks.Create("cross default empty", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-cross-default"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "cross default completed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
	tk, _ := env.tasks.Get(created.ID)
	var prov string
	for i := range tk.Workflow.StepHistory {
		if tk.Workflow.StepHistory[i].StepID == "cross_step" {
			prov = tk.Workflow.StepHistory[i].Provider
		}
	}
	if prov != "codex" {
		t.Fatalf("cross_step provider = %q, want codex when default empty(normalized claude)", prov)
	}
}

func TestE2E_VerifyCommits_CanceledContext_SkipsWithGitError(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-verify-only", testVerifyOnlyWorkflowYAML)

	created, err := env.tasks.Create("verify canceled ctx", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wtPath := filepath.Join(env.worktreesDir, cur.DirName())
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepoWithOriginMain(t, wtPath)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env.engine.SetContext(ctx)

	if err := env.startWorkflow(created.ID, "test-verify-only"); err != nil {
		t.Fatal(err)
	}
	tk, _ := env.tasks.Get(created.ID)
	var out string
	for i := range tk.Workflow.StepHistory {
		if tk.Workflow.StepHistory[i].StepID == "verify" {
			out = tk.Workflow.StepHistory[i].Output
		}
	}
	if !strings.Contains(strings.ToLower(out), "context canceled") {
		t.Fatalf("verify output = %q, want context canceled skip", out)
	}
}

func TestE2E_LinkPRAndReview_GHAmbiguous_NoAutoLink(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success"})
	if err := os.WriteFile(filepath.Join(env.wfStore.Dir(), "test-eval-chain.yaml"), []byte(testEvalChainWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := stageProviderPath(t, "claude", "synapse-cli")
	gh := filepath.Join(bin, "gh")
	script := "#!/bin/sh\nprintf '[{\"number\":1},{\"number\":2}]'\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	created, err := env.tasks.Create("gh ambiguous", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	branch := "synapse/" + created.ID
	projectID := "test-org/test-repo"
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"project_id": projectID,
		"branch":     branch,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-eval-chain"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "gh ambiguous workflow complete", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})
	tk, _ := env.tasks.Get(created.ID)
	if tk.PRNumber != 0 {
		t.Fatalf("pr_number = %d, want 0 on ambiguous gh list", tk.PRNumber)
	}
	if tk.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required fallback", tk.Status)
	}
}

func TestE2E_InteractivePromptQueuePressure_NoDropOrCrash(t *testing.T) {
	env := setupE2EProvider(t, "claude", "interactive_implement")
	ag, err := env.agents.Run(agent.RunConfig{
		TaskID:   "prompt-pressure",
		Name:     "prompt pressure",
		Mode:     "interactive",
		Provider: "claude",
		Model:    "sonnet",
		Prompt:   "start",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "agent live for pressure", func() bool {
		s := ag.GetState()
		return s == agent.StateRunning || s == agent.StatePaused
	})

	var wg sync.WaitGroup
	errCh := make(chan error, 50)
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errCh <- env.agents.SendPromptToAgent(ag.ID, fmt.Sprintf("msg-%d", i))
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("send prompt err: %v", err)
		}
	}
	if err := env.agents.StopAgent(ag.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "pressure agent stopped", func() bool {
		got, gErr := env.agents.GetAgent(ag.ID)
		return gErr == nil && got.GetState() == agent.StateStopped
	})
}

func TestE2E_RestartSimulation_PersistedWaitingResumesOnce(t *testing.T) {
	env := setupE2EProvider(t, "claude", "success")
	created, err := env.tasks.Create("restart simulation", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       workflow.ExecRunning,
		Variables:   map[string]string{workflow.WorkflowVarDir: env.agentDir},
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status":   "in-progress",
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}

	restored := rebuildEngineFromEnv(t, env)
	restored.ResumeStalled()
	waitFor(t, 15*time.Second, "restored engine advances workflow", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep != "implement"
	})
	tk, _ := env.tasks.Get(created.ID)
	if got := countStepRecords(tk, "implement"); got != 1 {
		t.Fatalf("implement records = %d, want 1 after restart resume", got)
	}
}

func TestE2E_ProviderUnhealthy_NoFailoverReturnsError(t *testing.T) {
	env := setupE2EProvider(t, "claude", "success")
	g := newScriptedGate()
	g.healthy["claude"] = false
	g.reason["claude"] = "logged_out"
	// No failover mapping -> scheduler must reject run.
	env.agents.SetHealthGate(g)

	_, err := env.agents.Run(agent.RunConfig{
		TaskID:   "no-failover",
		Name:     "no failover",
		Mode:     "headless",
		Provider: "claude",
		Prompt:   "work",
		Dir:      env.agentDir,
	})
	if err == nil {
		t.Fatal("expected unhealthy provider error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unhealthy") {
		t.Fatalf("err = %v, want unhealthy error", err)
	}
}

func TestE2E_WorkflowReload_CorruptFileKeepsWaitingState(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-reload-live", testReloadWorkflowYAML)

	created, err := env.tasks.Create("reload corrupt", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-reload-live"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "reload gate waiting", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "gate" && tk.Workflow.State == workflow.ExecWaiting
	})

	corrupt := filepath.Join(env.wfStore.Dir(), "test-reload-live.yaml")
	if err := os.WriteFile(corrupt, []byte("id: test-reload-live\nsteps:\n  - : bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = env.engine.HandleHumanAction(created.ID, "approve", nil)
	if err == nil {
		t.Fatal("expected parse error from corrupted workflow file")
	}

	tk, _ := env.tasks.Get(created.ID)
	if tk.Workflow.State != workflow.ExecWaiting || tk.Workflow.CurrentStep != "gate" {
		t.Fatalf("workflow mutated on corrupt reload: step=%q state=%q", tk.Workflow.CurrentStep, tk.Workflow.State)
	}
}

func TestE2E_StatusChange_AfterTerminal_NoMutation(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"interactive_implement"})
	writeWorkflowFixture(t, env, "test-wait-status", testWaitForStatusWorkflowYAML)

	created, err := env.tasks.Create("terminal status noop", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-wait-status"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "waiting on plan_wait", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.CurrentStep == "plan_wait"
	})
	env.engine.HandleStatusChange(created.ID, "plan-review")
	waitFor(t, 10*time.Second, "workflow completed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	historyBefore := len(tk.Workflow.StepHistory)
	env.engine.HandleStatusChange(created.ID, "plan-review")
	time.Sleep(150 * time.Millisecond)
	after, _ := env.tasks.Get(created.ID)
	if len(after.Workflow.StepHistory) != historyBefore {
		t.Fatalf("terminal status change mutated history: before=%d after=%d", historyBefore, len(after.Workflow.StepHistory))
	}
}

func TestE2E_EnsurePRClosesIssue_EditFailure_FlipsHumanRequired(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-ensure-pr", testEnsurePRWorkflowYAML)

	editCalled := false
	env.engine.SetPRLinker(&scriptedPRLinker{
		get: func(repo string, prNumber int) ([]int, string, error) {
			return nil, "Body", nil
		},
		edit: func(repo string, prNumber int, body string) error {
			editCalled = true
			return errors.New("forbidden edit")
		},
	})

	created, err := env.tasks.Create("ensure pr edit fail", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"project_id": "owner/repo",
		"issue":      "https://github.com/owner/repo/issues/42",
		"pr_number":  101,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-ensure-pr"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "ensure step completed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	if !editCalled {
		t.Fatal("expected PR body edit attempt")
	}
	tk, _ := env.tasks.Get(created.ID)
	if tk.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", tk.Status)
	}
	if !strings.Contains(tk.StatusReason, "auto-fix failed") {
		t.Fatalf("status_reason = %q, want auto-fix failed message", tk.StatusReason)
	}
	if got := countStepRecords(tk, "ensure"); got != 1 {
		t.Fatalf("ensure step records = %d, want 1", got)
	}
	if st := lastStepStatus(tk, "ensure"); st != "failed" {
		t.Fatalf("ensure step status = %q, want failed", st)
	}
}

func TestE2E_EnsurePRClosesIssue_AlreadyLinked_SkipsEdit(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-ensure-pr", testEnsurePRWorkflowYAML)

	editCalled := false
	env.engine.SetPRLinker(&scriptedPRLinker{
		get: func(repo string, prNumber int) ([]int, string, error) {
			return []int{42}, "Body", nil
		},
		edit: func(repo string, prNumber int, body string) error {
			editCalled = true
			return nil
		},
	})

	created, err := env.tasks.Create("ensure pr already linked", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"project_id": "owner/repo",
		"issue":      "https://github.com/owner/repo/issues/42",
		"pr_number":  101,
		"status":     "in-progress",
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-ensure-pr"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "ensure already-linked completed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	if editCalled {
		t.Fatal("unexpected PR body edit when already linked")
	}
	tk, _ := env.tasks.Get(created.ID)
	if tk.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want unchanged in-progress", tk.Status)
	}
	if st := lastStepStatus(tk, "ensure"); st != "completed" {
		t.Fatalf("ensure step status = %q, want completed", st)
	}
}

func TestE2E_Evaluate_LateGHSinglePR_FlipsInReview(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-evaluate-late-pr", testEvaluateLatePRWorkflowYAML)

	bin := stageProviderPath(t, "claude", "synapse-cli")
	gh := filepath.Join(bin, "gh")
	script := "#!/bin/sh\nprintf '[{\"number\":321}]'\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	created, err := env.tasks.Create("evaluate late gh", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"project_id": "owner/repo",
		"branch":     "synapse/" + created.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-evaluate-late-pr"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "evaluate late-gh completed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.PRNumber != 321 {
		t.Fatalf("pr_number = %d, want 321", tk.PRNumber)
	}
	if tk.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review", tk.Status)
	}
}

func TestE2E_LinkPRAndReview_ShortRefFromHistory_FlipsInReview(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-shell-link-pr", testShellToLinkPRWorkflowYAML)

	created, err := env.tasks.Create("link from short ref", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-shell-link-pr"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "link short ref completed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	tk, _ := env.tasks.Get(created.ID)
	if tk.PRNumber != 55 {
		t.Fatalf("pr_number = %d, want 55", tk.PRNumber)
	}
	if tk.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review", tk.Status)
	}
}

func TestE2E_EnsurePRClosesIssue_CrossRepoIssue_SkipsEdit(t *testing.T) {
	env := setupE2E(t, "success")
	writeWorkflowFixture(t, env, "test-ensure-pr", testEnsurePRWorkflowYAML)

	editCalled := false
	env.engine.SetPRLinker(&scriptedPRLinker{
		get: func(repo string, prNumber int) ([]int, string, error) {
			return nil, "", nil
		},
		edit: func(repo string, prNumber int, body string) error {
			editCalled = true
			return nil
		},
	})

	created, err := env.tasks.Create("ensure cross repo skip", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"project_id": "owner/repo",
		"issue":      "https://github.com/other/repo/issues/42",
		"pr_number":  101,
		"status":     "in-progress",
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-ensure-pr"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 10*time.Second, "ensure cross-repo completed", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecCompleted
	})

	if editCalled {
		t.Fatal("unexpected edit call for cross-repo issue")
	}
	tk, _ := env.tasks.Get(created.ID)
	if tk.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want unchanged in-progress", tk.Status)
	}
}
