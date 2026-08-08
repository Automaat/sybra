package workflow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestHandleAgentComplete_CheckpointFailedParksWithoutRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "todo",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "triage",
			State:       ExecWaiting,
		},
	})
	setWorkflowAgentRoute(t, tasks, "t1", "checkpoint-agent", "triage")

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID:          "checkpoint-agent",
		Provider:         "claude",
		Success:          false,
		EscalationReason: "checkpoint_failed",
	})

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("replacement agent starts = %d, want 0", got)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason != "checkpoint_failed: checkpoint commit failed — no durable checkpoint state created" {
		t.Fatalf("status reason = %q", got.StatusReason)
	}
	if got.Workflow == nil {
		t.Fatal("workflow = nil")
	}
	if got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %q, want completed", got.Workflow.State)
	}
	if got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow current step = %q, want empty", got.Workflow.CurrentStep)
	}
	if got.Workflow.CountStep("triage") != 1 {
		t.Fatalf("step count = %d, want 1", got.Workflow.CountStep("triage"))
	}
	if len(got.Workflow.StepHistory) != 1 || got.Workflow.StepHistory[0].Status != "failed" {
		t.Fatalf("step history = %+v, want one failed triage record", got.Workflow.StepHistory)
	}
	if _, tracked := lookupWorkflowAgentRoute(t, engine, "t1", "checkpoint-agent"); tracked {
		t.Fatal("checkpoint_failed agent step mapping was not cleared")
	}
}

func TestHandleHumanAction_NotWaiting(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecRunning,
			Variables:   make(map[string]string),
		},
	})

	err := engine.HandleHumanAction("t1", "approve", nil)
	if err == nil {
		t.Fatal("expected error for non-waiting task")
	}
	var ce *ClientError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ClientError so httpapi surfaces it as a 4xx instead of a sanitized 500, got %T: %v", err, err)
	}
	if ce.HTTPStatus() != http.StatusConflict {
		t.Fatalf("status = %d, want %d", ce.HTTPStatus(), http.StatusConflict)
	}
}

func TestHandleHumanAction_InvalidActionAlreadyWaitingDoesNotMutate(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "plan-review",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{"existing": "value"},
			StepHistory: []StepRecord{{StepID: "plan", Status: "completed"}},
		},
	})

	err := engine.HandleHumanAction("t1", "bogus", nil)
	if err == nil || !strings.Contains(err.Error(), "invalid human action") {
		t.Fatalf("HandleHumanAction error = %v, want invalid human action", err)
	}
	var ce *ClientError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ClientError so httpapi surfaces it as a 4xx instead of a sanitized 500, got %T: %v", err, err)
	}
	if ce.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", ce.HTTPStatus(), http.StatusBadRequest)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.CurrentStep != "review_plan" {
		t.Fatalf("CurrentStep = %q, want review_plan", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Fatalf("State = %q, want %q", got.Workflow.State, ExecWaiting)
	}
	if _, ok := got.Workflow.Variables["human_action"]; ok {
		t.Fatal("human_action var was set for rejected action")
	}
	if got.Workflow.Variables["existing"] != "value" {
		t.Fatalf("existing var = %q, want value", got.Workflow.Variables["existing"])
	}
	if len(got.Workflow.StepHistory) != 1 || got.Workflow.StepHistory[0].StepID != "plan" {
		t.Fatalf("StepHistory changed: %+v", got.Workflow.StepHistory)
	}
}

func TestAdvanceStep_UnknownStepID(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// An advance for a step that does not match the workflow's current step
	// is a stale completion (e.g. a duplicate agent from a ResumeStalled race,
	// or a stray callback after the workflow advanced). The engine must
	// silently no-op instead of crashing or mutating step history — that
	// guard is what stops a second plan agent from driving review_plan into
	// ExecFailed when its delayed completion arrives after the human gate.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "nonexistent-step", Status: "completed"}); err != nil {
		t.Fatalf("stale stepID should be a no-op, got err: %v", err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "triage" {
		t.Errorf("CurrentStep = %q, want unchanged triage", ti.Workflow.CurrentStep)
	}
	if got := len(ti.Workflow.StepHistory); got != 0 {
		t.Errorf("step history len = %d, want 0 — stale advance must not append", got)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("state = %q, want ExecWaiting (unchanged)", ti.Workflow.State)
	}
}

func TestAdvanceStep_TaskWithoutWorkflow(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	if err == nil {
		t.Fatal("expected error for task without workflow")
	}
}

func TestShellStep_ExecutesCommand(t *testing.T) {
	// Test the shell step directly using a simple echo command.
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "shell1",
		Type: StepShell,
		Config: StepConfig{
			Command: "echo hello-from-shell",
		},
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1", Title: "test"},
		Step: *step,
		Vars: make(map[string]string),
	}

	output, err := engine.execShell(step, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if output.Status != "completed" {
		t.Fatalf("expected completed, got %q", output.Status)
	}
	if output.Output != "hello-from-shell\n" {
		t.Fatalf("expected 'hello-from-shell\\n', got %q", output.Output)
	}
}

// TestShellStep_StdinReaderExitsOnEOF covers a subtle deadlock: execShell
// does not wire stdin, so commands that call `read` or `cat` inherit a
// nil/closed stdin and should exit immediately with EOF. A regression that
// passed through os.Stdin (or left the pipe dangling) would cause the shell
// step to hang for the full shellTimeout (30s). The 5-second deadline here
// proves the command exits promptly.
func TestShellStep_StdinReaderExitsOnEOF(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "stdin-reader",
		Type: StepShell,
		Config: StepConfig{
			// `read` exits non-zero on EOF. `cat` exits 0 immediately since
			// its stdin is empty. Both should be fast.
			Command: "cat",
		},
	}
	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	done := make(chan error, 1)
	go func() {
		_, err := engine.execShell(step, ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execShell: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execShell hung on stdin-reading command — sybra provides no stdin, `cat` should EOF immediately")
	}
}

// TestShellStep_ContextCancelKillsCommand verifies that cancelling the
// engine's parent context terminates a long-running shell step promptly
// rather than waiting out the 30s shellTimeout. execShell derives its own
// context via context.WithTimeout(e.ctx, shellTimeout); cancelling e.ctx
// must propagate down and kill the subprocess via exec.CommandContext.
// A regression that used context.Background() instead of e.ctx would
// leave the command running after app shutdown.
func TestShellStep_ContextCancelKillsCommand(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	parentCtx, cancel := context.WithCancel(context.Background())
	engine.SetContext(parentCtx)

	marker := filepath.Join(t.TempDir(), "started")
	step := &Step{
		ID:   "long-sleep",
		Type: StepShell,
		Config: StepConfig{
			Command: fmt.Sprintf("touch %q && sleep 30", marker),
		},
	}
	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	// Cancel once the subprocess has actually started (touched its marker);
	// the sleep would otherwise run 30 seconds.
	go func() {
		if pollUntil(5*time.Second, 10*time.Millisecond, func() bool {
			_, err := os.Stat(marker)
			return err == nil
		}) {
			cancel()
		}
	}()

	start := time.Now()
	output, err := engine.execShell(step, ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("execShell: %v", err)
	}
	// Killed subprocess is "failed", not "completed".
	if output.Status != "failed" {
		t.Errorf("status = %q, want failed (subprocess killed by ctx cancel)", output.Status)
	}
	// Must return within a handful of seconds — certainly well under 30s
	// shellTimeout. 10s is plenty of slack for slow CI.
	if elapsed > 10*time.Second {
		t.Errorf("execShell took %v after ctx cancel — should return promptly", elapsed)
	}
}

func TestShellStep_FailingCommandSetsStatusFailed(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "shell1",
		Type: StepShell,
		Config: StepConfig{
			Command: "exit 1",
		},
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	output, err := engine.execShell(step, ctx)
	if err != nil {
		t.Fatal(err) // execShell doesn't return error on command failure
	}
	if output.Status != "failed" {
		t.Fatalf("expected failed, got %q", output.Status)
	}
}

func TestShellStep_EmptyRenderedDirFailsClosed(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	step := &Step{
		ID:   "shell-empty-dir",
		Type: StepShell,
		Config: StepConfig{
			Command: "pwd",
			Dir:     "{{getvar .Vars \"missing_dir\"}}",
		},
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: make(map[string]string),
	}

	_, err := engine.execShell(step, ctx)
	if err == nil {
		t.Fatal("expected error for empty rendered dir")
	}
	if !strings.Contains(err.Error(), "resolved to empty path") {
		t.Fatalf("err = %v, want empty-path failure", err)
	}
}

func TestHandleAgentComplete_FailedQuarantinedWorkflowIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Run through the lifecycle until evaluate quarantines the task.
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed"})
	agents.SimulateComplete("t1")
	_ = engine.AdvanceStep("t1", StepOutput{StepID: "evaluate", Status: "completed"})

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecFailed {
		t.Fatalf("precondition: expected failed quarantine, got %q", ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "" {
		t.Fatalf("precondition: expected empty current step after completion, got %q", ti.Workflow.CurrentStep)
	}
	historyBefore := len(ti.Workflow.StepHistory)

	// Another agent complete on an already-failed workflow should not
	// start new agents, mutate step history, or record an error.
	callsBefore := agents.CallCount()
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "stale-agent", Result: "late result", Success: true})

	if agents.CallCount() != callsBefore {
		t.Error("HandleAgentComplete on completed workflow should not start new agents")
	}

	tiAfter, _ := tasks.GetTask("t1")
	if got := len(tiAfter.Workflow.StepHistory); got != historyBefore {
		t.Errorf("StepHistory len = %d, want %d — stale completion must not append",
			got, historyBefore)
	}
	if tiAfter.Workflow.State != ExecFailed {
		t.Errorf("State = %q, want ExecFailed — stale completion must not mutate state",
			tiAfter.Workflow.State)
	}
	if tiAfter.Workflow.CurrentStep != "" {
		t.Errorf("CurrentStep = %q, want empty — stale completion must not mutate current step",
			tiAfter.Workflow.CurrentStep)
	}
}

func TestHandleAgentComplete_UntrackedRoleMismatchDoesNotAdvanceCurrentStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
		AgentRuns: []AgentRunInfo{
			{AgentID: "plan-agent", Role: "plan"},
		},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID: "plan-agent",
		Result:  "late plan completion",
		Success: true,
	})

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "implement" {
		t.Fatalf("CurrentStep = %q, want implement — plan completion must not satisfy implementation step",
			ti.Workflow.CurrentStep)
	}
	if len(ti.Workflow.StepHistory) != 0 {
		t.Fatalf("StepHistory = %+v, want no recorded implementation completion", ti.Workflow.StepHistory)
	}
}

func TestHandleAgentComplete_UnverifiedSkillRetriesWithInjectedSkill(t *testing.T) {
	store := newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      provider: codex
      prompt: "Run /sybra-test now."
      import_sidecar:
        from: '{{getvar .Vars "_dir"}}/.sybra-plan-{{.Task.ID}}.md'
        kind: plan
        required: true
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	recorder := &recordingArtifactRecorder{}
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetArtifactRecorder(recorder)

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Plan:      "# fake first pass\n",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables: map[string]string{
				WorkflowVarDir: t.TempDir(),
			},
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            "agent-1",
			Role:               "plan-critic",
			Provider:           "codex",
			RequestedSkill:     "sybra-test",
			SkillExecutionMode: "native",
			SkillConformance:   "unverified",
			TurnCount:          7,
		}},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Result: "done", Success: true})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ti.Workflow.CurrentStep; got != "run" {
		t.Fatalf("CurrentStep = %q, want run", got)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "1" {
		t.Fatalf("skill receipt retry var = %q, want 1", got)
	}
	if len(ti.Workflow.StepHistory) != 0 {
		t.Fatalf("StepHistory = %+v, want no recorded completion before retry", ti.Workflow.StepHistory)
	}
	if len(agents.calls) != 1 {
		t.Fatalf("StartAgent calls = %d, want 1 retry", len(agents.calls))
	}
	if !agents.calls[0].Assignment.ForceInjectedSkill {
		t.Fatal("retry assignment did not force injected skill delivery")
	}
	if !agents.calls[0].Assignment.SkillRecoveryAttempt {
		t.Fatal("retry assignment missing recovery-attempt marker")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.puts) != 1 {
		t.Fatalf("diagnostic artifacts = %+v, want one preserved first-pass sidecar", recorder.puts)
	}
	if recorder.puts[0].name != "skill-receipt-first-run-plan.md" {
		t.Fatalf("diagnostic artifact name = %q, want skill-receipt-first-run-plan.md", recorder.puts[0].name)
	}
	if recorder.puts[0].content != "# fake first pass\n" {
		t.Fatalf("diagnostic artifact content = %q, want first-pass plan", recorder.puts[0].content)
	}
}

func TestHandleAgentComplete_UnverifiedSkillAfterRetryEscalatesHumanRequired(t *testing.T) {
	store := newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      provider: codex
      prompt: "Run /sybra-test now."
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	var completed []CompletionInfo
	engine.SetOnComplete(func(info CompletionInfo) {
		completed = append(completed, info)
	})

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables: map[string]string{
				skillReceiptRecoveryKey("run"): "1",
			},
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            "agent-2",
			Role:               "plan-critic",
			Provider:           "codex",
			RequestedSkill:     "sybra-test",
			SkillExecutionMode: "injected",
			SkillConformance:   "unverified",
			TurnCount:          7,
		}},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID: "agent-2",
		Result: `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":"## Test Failures\n\nClassification: product_bug: repo does not compile\n\nObserved output:\n` +
			"```text\npkg/api-server/resource_inspect_endpoints.go:14: dangling import\n```" +
			`"}`,
		Success: true,
	})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "blocked" {
		t.Fatalf("Status = %q, want blocked quarantine", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "no conformance receipt after automatic recovery retry") {
		t.Fatalf("StatusReason = %q, want receipt-retry exhaustion", ti.StatusReason)
	}
	if !strings.Contains(ti.StatusReason, "product_bug: repo does not compile") {
		t.Fatalf("StatusReason = %q, want parsed verdict summary", ti.StatusReason)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("skill receipt retry var = %q, want cleared after exhaustion", got)
	}
	if len(agents.calls) != 0 {
		t.Fatalf("StartAgent calls = %d, want no third attempt", len(agents.calls))
	}
	if ti.Workflow.State != ExecCompleted {
		t.Fatalf("Workflow.State = %q, want %q so a later recovery trigger is not blocked by ErrWorkflowAlreadyActive", ti.Workflow.State, ExecCompleted)
	}
	if ti.Workflow.CurrentStep != "" {
		t.Fatalf("Workflow.CurrentStep = %q, want empty", ti.Workflow.CurrentStep)
	}
	if len(completed) != 1 {
		t.Fatalf("workflow completion callbacks = %d, want 1 exhausted completion for downstream recovery", len(completed))
	}
	if completed[0].TaskID != "t1" || completed[0].WorkflowID != "skill-receipt" {
		t.Fatalf("completion = %+v, want task/workflow ids for exhausted run", completed[0])
	}
	if got := completed[0].Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("completion retry var = %q, want cleared before downstream recovery sees completion", got)
	}
}

func TestHandleAgentComplete_UnverifiedSkillAfterRetryContinuesWithImportedSidecar(t *testing.T) {
	store := newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: review
      mode: headless
      provider: codex
      prompt: "Run /adversarial-review now."
      import_sidecar:
        from: '{{getvar .Vars "_dir"}}/.sybra-review-{{.Task.ID}}.md'
        kind: code_review
        required: true
    next:
      - goto: require_review
  - id: require_review
    name: Require Review
    type: require_sidecar
    config:
      sidecar: code_review
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".sybra-review-t1.md"), []byte("Review Verdict: CLEAN\n\nNo findings.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables: map[string]string{
				WorkflowVarDir:                 dir,
				skillReceiptRecoveryKey("run"): "1",
			},
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            "agent-2",
			Role:               "review",
			Provider:           "codex",
			RequestedSkill:     "adversarial-review",
			SkillExecutionMode: "injected",
			SkillConformance:   "unverified",
			TurnCount:          7,
		}},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID: "agent-2",
		Result:  "review complete without receipt",
		Success: true,
	})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "done" {
		t.Fatalf("Status = %q, want done", ti.Status)
	}
	if !strings.Contains(ti.CodeReview, "Review Verdict: CLEAN") {
		t.Fatalf("CodeReview = %q, want imported review sidecar", ti.CodeReview)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("skill receipt retry var = %q, want cleared after sidecar continuation", got)
	}
	if len(agents.calls) != 0 {
		t.Fatalf("StartAgent calls = %d, want no third attempt", len(agents.calls))
	}
}

// TestHandleAgentComplete_UnverifiedSkillExhaustionAllowsFreshWorkflowStart
// covers the human-review recovery handoff: once skill-receipt exhaustion
// marks a task human-required, a subsequent recovery attempt must be able to
// start a fresh workflow instance rather than fail with
// ErrWorkflowAlreadyActive against the exhausted, never-finalized Execution
// (the bug in #5ba88ecc — a later genuinely passing run stayed parked at
// human-required because the stale Execution was still "active").
func TestHandleAgentComplete_UnverifiedSkillExhaustionAllowsFreshWorkflowStart(t *testing.T) {
	store := newInlineTestStore(t, "skill-receipt", `id: skill-receipt
name: Skill Receipt
trigger:
  on: task.created
steps:
  - id: run
    name: Run
    type: run_agent
    config:
      role: plan-critic
      mode: headless
      provider: codex
      prompt: "Run /sybra-test now."
    next:
      - goto: done
  - id: done
    name: Done
    type: set_status
    config:
      status: done
`)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentMode: "headless",
		Workflow: &Execution{
			WorkflowID:  "skill-receipt",
			CurrentStep: "run",
			State:       ExecWaiting,
			Variables: map[string]string{
				skillReceiptRecoveryKey("run"): "1",
			},
		},
		AgentRuns: []AgentRunInfo{{
			AgentID:            "agent-2",
			Role:               "plan-critic",
			Provider:           "codex",
			RequestedSkill:     "sybra-test",
			SkillExecutionMode: "injected",
			SkillConformance:   "unverified",
			TurnCount:          7,
		}},
	})

	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-2", Result: "still no receipt", Success: true})

	if err := engine.StartWorkflow("t1", "skill-receipt"); err != nil {
		t.Fatalf("StartWorkflow after exhaustion = %v, want nil (fresh recovery trigger must not be rejected as already active)", err)
	}

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ti.Workflow.Variables[skillReceiptRecoveryKey("run")]; got != "" {
		t.Fatalf("skill receipt retry var = %q, want a fresh budget on the new Execution", got)
	}
	if ti.Workflow.State == ExecCompleted {
		t.Fatalf("Workflow.State = %q, want the fresh restart to be running/waiting again", ti.Workflow.State)
	}
}

// TestAdvanceStep_EmptyStepIDIsNoop covers the direct-call variant: a caller
// that passes an empty StepID (e.g. because t.Workflow.CurrentStep was reset
// to "" by a previous completion) used to error with "step not found in
// workflow", which the agent-complete path would log as ERROR and still
// persist via RecordStep. The guard must return nil and leave state intact.
func TestAdvanceStep_EmptyStepIDIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}
	// Force the workflow into the pathological state observed in prod:
	// state=completed, current_step="" — mirrors what resolveNext leaves
	// behind when a terminal step evaluates to goto: "".
	ti, _ := tasks.GetTask("t1")
	ti.Workflow.State = ExecCompleted
	ti.Workflow.CurrentStep = ""
	if err := tasks.SetWorkflow("t1", ti.Workflow); err != nil {
		t.Fatal(err)
	}
	historyBefore := len(ti.Workflow.StepHistory)

	err := engine.AdvanceStep("t1", StepOutput{StepID: "", Status: "completed"})
	if err != nil {
		t.Errorf("AdvanceStep with empty StepID = %v, want nil (no-op)", err)
	}

	tiAfter, _ := tasks.GetTask("t1")
	if got := len(tiAfter.Workflow.StepHistory); got != historyBefore {
		t.Errorf("StepHistory len = %d, want %d — empty-step advance must not append",
			got, historyBefore)
	}
	if tiAfter.Workflow.State != ExecCompleted {
		t.Errorf("State = %q, want ExecCompleted", tiAfter.Workflow.State)
	}
}

// TestAdvanceStep_FailedWorkflowIsNoop pins the other terminal state:
// workflows that hit ExecFailed also must refuse further advances.
func TestAdvanceStep_FailedWorkflowIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "triage",
			State:       ExecFailed,
			Variables:   make(map[string]string),
		},
	})

	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"})
	if err != nil {
		t.Errorf("AdvanceStep on failed workflow = %v, want nil (no-op)", err)
	}
	if agents.CallCount() != 0 {
		t.Errorf("agents.CallCount = %d, want 0 — failed workflow must not spawn", agents.CallCount())
	}
}

func TestHandleStatusChange_AdvancesRunAgentWhenWaitForStatusMatches(t *testing.T) {
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
	if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
		t.Fatal(err)
	}

	// Before the status flips, we're still in the plan run_agent step.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "plan" {
		t.Fatalf("precondition: expected plan step, got %q", ti.Workflow.CurrentStep)
	}

	// The plan agent flips the task status — engine should advance to
	// review_plan without the agent process having to exit.
	tasks.SetStatus("t1", "plan-review")
	engine.HandleStatusChange("t1", "plan-review")

	ti, _ = tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("CurrentStep = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", ti.Workflow.State)
	}
}

func TestHandleStatusChange_NoOp(t *testing.T) {
	tests := []struct {
		name      string
		newStatus string
		// mutate lets each case set up its own pre-state after the
		// default "workflow started, sitting in plan step" arrangement.
		mutate func(tasks *memTasks)
	}{
		{
			name:      "status does not match wait_for_status",
			newStatus: "todo",
		},
		{
			name:      "current step is not a run_agent",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow.CurrentStep = "review_plan"
				_ = tasks.SetWorkflow("t1", ti.Workflow)
			},
		},
		{
			name:      "workflow already completed",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow.State = ExecCompleted
				_ = tasks.SetWorkflow("t1", ti.Workflow)
			},
		},
		{
			name:      "task has no workflow",
			newStatus: "plan-review",
			mutate: func(tasks *memTasks) {
				ti, _ := tasks.GetTask("t1")
				ti.Workflow = nil
				tasks.Put(ti)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStoreWith(t, "test-plan-reuse.yaml")
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())

			tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})
			if err := engine.StartWorkflow("t1", "test-plan-reuse"); err != nil {
				t.Fatal(err)
			}

			if tt.mutate != nil {
				tt.mutate(tasks)
			}

			// Snapshot the current step so we can detect any advance.
			before, _ := tasks.GetTask("t1")
			wantStep := ""
			if before.Workflow != nil {
				wantStep = before.Workflow.CurrentStep
			}

			engine.HandleStatusChange("t1", tt.newStatus)

			after, _ := tasks.GetTask("t1")
			gotStep := ""
			if after.Workflow != nil {
				gotStep = after.Workflow.CurrentStep
			}
			if gotStep != wantStep {
				t.Errorf("CurrentStep changed to %q, want %q (no advance)", gotStep, wantStep)
			}
		})
	}
}

func TestHandleStatusChange_UnknownTaskDoesNotPanic(t *testing.T) {
	store := newTestStoreWith(t, "test-plan-reuse.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Act — must not panic even though the task was never registered.
	engine.HandleStatusChange("ghost", "plan-review")
}

// TestHandleAgentComplete_WaitHumanWithoutActionIsNoop is the defense-in-depth
// guard for the same bug. If a stray agent completion slips past the stale-
// step check and lands on a wait_human step without a human_action var set
// (e.g. an untracked legacy agent where HandleAgentComplete falls back to
// CurrentStep), AdvanceStep must still refuse to run resolveNext. Otherwise
// the workflow would fail on an unmatched transition and permanently seal
// the human review gate.
func TestHandleAgentComplete_WaitHumanWithoutActionIsNoop(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// Put a task directly at the wait_human step with no agent tracked.
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "plan-review",
		AgentMode: "interactive",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "review_plan",
			State:       ExecWaiting,
			Variables:   map[string]string{},
		},
	})

	// Agent callback arrives for the current (wait_human) step with no
	// human_action set. Must be a no-op.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "untracked-legacy-agent", Result: "unexpected result", Success: true})

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.State != ExecWaiting {
		t.Errorf("state = %q, want ExecWaiting — stray completion on wait_human must not fail the workflow",
			ti.Workflow.State)
	}
	if ti.Workflow.CurrentStep != "review_plan" {
		t.Errorf("current_step = %q, want review_plan", ti.Workflow.CurrentStep)
	}
	if got := len(ti.Workflow.StepHistory); got != 0 {
		t.Errorf("step_history len = %d, want 0 — stray wait_human completion must not append", got)
	}

	// Rejection still works after the defense kicks in.
	if err := engine.HandleHumanAction("t1", "approve", nil); err != nil {
		t.Fatalf("HandleHumanAction approve: %v", err)
	}
}

func TestExecuteSteps_CycleDetection(t *testing.T) {
	store := newTestStoreWith(t, "test-cycle.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "cycle1", Status: "todo", AgentMode: "headless"})

	err := engine.StartWorkflow("cycle1", "test-cycle")
	if err == nil {
		t.Fatal("expected error for cyclic workflow, got nil")
	}

	cycleErr, ok := errors.AsType[*CycleError](err)
	if !ok {
		t.Fatalf("expected *CycleError, got %T: %v", err, err)
	}
	if cycleErr.StepID == "" {
		t.Error("CycleError.StepID is empty")
	}
	if cycleErr.At <= cycleErr.FirstAt {
		t.Errorf("CycleError.At (%d) should be > FirstAt (%d)", cycleErr.At, cycleErr.FirstAt)
	}
}

// TestExecuteSteps_VerifyCommitsParkDoesNotComplete is the regression test for
// the Copilot finding: a deferred verify_commits must NOT complete the workflow,
// because OnWorkflowComplete cascades on the (still in-progress) status and would
// re-dispatch simple-task-implement — whose execRunAgent StopAgentsForTask would
// kill the still-running sibling. executeSteps must return a nil completion
// (parked, ExecWaiting), never a CompletionInfo.
func TestExecuteSteps_VerifyCommitsParkDoesNotComplete(t *testing.T) {
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var impl *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-implement" {
			impl = &defs[i]
			break
		}
	}
	if impl == nil {
		t.Fatal("simple-task-implement builtin not found")
		return
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID: "t1", Status: "in-progress",
		Workflow: &Execution{WorkflowID: "simple-task-implement", CurrentStep: "verify_commits", State: ExecRunning},
	})
	agents := newMockAgents()
	agents.mu.Lock()
	agents.running["t1"] = "sibling"
	agents.mu.Unlock()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false /* no commits */), ok: true})

	wf := &Execution{WorkflowID: "simple-task-implement", CurrentStep: "verify_commits", State: ExecRunning}
	wf.RecordStep(StepRecord{StepID: "implement", Status: "failed", AgentID: "completer"})

	verifyStep := impl.StepByID("verify_commits")
	if verifyStep == nil {
		t.Fatal("verify_commits step not found in simple-task-implement")
		return
	}
	comp, err := engine.executeSteps("t1", impl, verifyStep, wf)
	if err != nil {
		t.Fatalf("executeSteps: %v", err)
	}
	if comp != nil {
		t.Errorf("executeSteps returned a completion (workflow finished → cascade would re-dispatch over the sibling); want nil (parked)")
	}
	if wf.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", wf.State)
	}
	if wf.CurrentStep != "implement" {
		t.Errorf("CurrentStep = %q, want implement (re-armed for ResumeStalled)", wf.CurrentStep)
	}
}

func TestAdvanceStep_ImplementHumanRequiredGitHubAuthParksForRetry(t *testing.T) {
	store := newStoreWithBuiltin(t, "simple-task-implement")
	tasks := newMemTasks()
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables:   map[string]string{},
	}
	reason := "push failed: X Failed to log in to github.com using token (GH_TOKEN)\n- The token in GH_TOKEN is invalid."
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: reason,
		AgentMode:    "headless",
		Workflow:     wf,
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		Status:  "completed",
		Output:  reason,
		AgentID: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
	if got.StatusReason != implementPushRetryStatusReason {
		t.Fatalf("status_reason = %q, want %q", got.StatusReason, implementPushRetryStatusReason)
	}
	if got.Workflow == nil {
		t.Fatal("workflow missing")
	}
	if got.Workflow.CurrentStep != "implement" {
		t.Errorf("CurrentStep = %q, want implement", got.Workflow.CurrentStep)
	}
	if got.Workflow.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", got.Workflow.State)
	}
	if got.Workflow.Variables[implementPushAttemptsVar] != "1" {
		t.Errorf("%s = %q, want 1", implementPushAttemptsVar, got.Workflow.Variables[implementPushAttemptsVar])
	}
	if _, ok := workflowRetryAfter(got.Workflow); !ok {
		t.Errorf("%s not set to a valid retry timestamp", workflowRetryAfterVar)
	}
}

func TestAdvanceStep_ImplementGitHubAuthRetryCapFallsThroughToHumanRequired(t *testing.T) {
	store := newStoreWithBuiltin(t, "simple-task-implement")
	tasks := newMemTasks()
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables: map[string]string{
			implementPushAttemptsVar: strconv.Itoa(maxImplementPushRetries),
		},
	}
	reason := "push failed: gh auth status: token is invalid"
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: reason,
		AgentMode:    "headless",
		Workflow:     wf,
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		Status:  "completed",
		Output:  reason,
		AgentID: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason != reason {
		t.Fatalf("status_reason = %q, want original reason %q", got.StatusReason, reason)
	}
	if got.Workflow == nil || got.Workflow.State != ExecCompleted || got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want completed terminal workflow", got.Workflow)
	}
}

func TestAdvanceStep_ImplementNonGitHubAuthHumanRequiredFallsThrough(t *testing.T) {
	store := newStoreWithBuiltin(t, "simple-task-implement")
	tasks := newMemTasks()
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecWaiting,
		Variables:   map[string]string{},
	}
	reason := "application auth provider rejected invalid token fixture"
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "human-required",
		StatusReason: reason,
		AgentMode:    "headless",
		Workflow:     wf,
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "implement",
		Status:  "completed",
		Output:  reason,
		AgentID: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.Workflow == nil || got.Workflow.State != ExecCompleted || got.Workflow.CurrentStep != "" {
		t.Fatalf("workflow = %+v, want completed terminal workflow", got.Workflow)
	}
}

func TestAdvanceStep_MarkReviewedAfterReviewRole(t *testing.T) {
	// After a run_agent step with role=review completes successfully,
	// the task must be marked reviewed so re-triggered workflows skip code_review.
	store := newTestStoreWith(t, "test-review-fix.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Todo, AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-review-fix"); err != nil {
		t.Fatal(err)
	}

	// implement → maybe_review → code_review
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "implement", Status: "completed", AgentID: "a1"}); err != nil {
		t.Fatal(err)
	}
	// Workflow should now be waiting at code_review.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow.CurrentStep != "code_review" {
		t.Fatalf("expected code_review step, got %q", ti.Workflow.CurrentStep)
	}

	// Complete code_review (role=review) → must mark reviewed.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "code_review", Status: "completed", AgentID: "a2", Output: "review done"}); err != nil {
		t.Fatal(err)
	}

	ti, _ = tasks.GetTask("t1")
	if !ti.Reviewed {
		t.Error("task.Reviewed = false after review-role step completed; want true")
	}
}

// TestAdvanceStep_WorkflowDefinitionDeletedMidRun covers the case where a
// workflow YAML file is removed from disk while an execution is in flight.
// loadAdvanceContext re-reads the definition from the store for every
// AdvanceStep call, so a deleted file must surface a clear error instead of
// panicking or silently reusing stale state. The task's workflow reference
// stays put — the caller decides whether to reset it.
func TestAdvanceStep_WorkflowDefinitionDeletedMidRun(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: taskstatus.Todo, AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// The definition file disappears (user-edit, git clean, rm -rf).
	if err := store.Delete("test-simple"); err != nil {
		t.Fatalf("Delete definition: %v", err)
	}

	agents.SimulateComplete("t1")
	err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed", Output: "triaged"})
	if err == nil {
		t.Fatal("AdvanceStep after definition delete returned nil; expected error")
	}
	if !strings.Contains(err.Error(), "test-simple") && !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error should reference the missing workflow; got %q", err)
	}

	// Task workflow reference must remain intact so the caller can inspect /
	// recover from the error rather than silently losing state.
	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil {
		t.Error("task.Workflow was cleared on definition-delete error; callers need it for recovery")
	}
	if ti.Workflow.WorkflowID != "test-simple" {
		t.Errorf("task.Workflow.WorkflowID = %q, want %q", ti.Workflow.WorkflowID, "test-simple")
	}
}
