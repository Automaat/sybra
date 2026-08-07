package workflow

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/prompteval"
)

func TestExecRunAgent_DefaultModeAndModel(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})

	step := &Step{
		ID:   "agent1",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "triage",
			Prompt: "test prompt",
			// Mode and Model intentionally empty.
		},
	}

	wfExec := &Execution{
		WorkflowID: "test-simple",
		State:      ExecRunning,
		Variables:  make(map[string]string),
	}

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Step: *step,
		Vars: wfExec.Variables,
	}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	call := agents.LastCall()
	if call.Mode != "headless" {
		t.Errorf("expected default mode 'headless', got %q", call.Mode)
	}
	if call.Model != "sonnet" {
		t.Errorf("expected default model 'sonnet', got %q", call.Model)
	}
}

func TestResolveRunAgentModel(t *testing.T) {
	t.Parallel()

	ctx := TemplateContext{
		Task: TaskInfo{ID: "t1"},
		Vars: map[string]string{},
	}

	if got := resolveRunAgentModel("", ctx); got != "sonnet" {
		t.Fatalf("resolveRunAgentModel(empty) = %q, want sonnet", got)
	}

	ctx.Vars[verifyRetryModelVar] = "expensive"
	model := `  {{if getvar .Vars "verify_retry_model"}}{{getvar .Vars "verify_retry_model"}}{{else}}cheap{{end}}  `
	if got := resolveRunAgentModel(model, ctx); got != "expensive" {
		t.Fatalf("resolveRunAgentModel(template with %s) = %q, want expensive", verifyRetryModelVar, got)
	}

	delete(ctx.Vars, verifyRetryModelVar)
	if got := resolveRunAgentModel(model, ctx); got != "cheap" {
		t.Fatalf("resolveRunAgentModel(template without %s) = %q, want cheap", verifyRetryModelVar, got)
	}
}

func TestExecRunAgent_PersistsPreparedWorktreeDir(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	step := &Step{
		ID:   "code_review",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:          "review",
			Prompt:        "review",
			NeedsWorktree: true,
		},
	}
	wfExec := &Execution{
		WorkflowID: "test-simple",
		State:      ExecRunning,
		Variables:  map[string]string{},
	}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(os.TempDir(), "sybra-test-t1")
	if got.Workflow.Variables[WorkflowVarDir] != wantDir {
		t.Fatalf("%s = %q, want %q", WorkflowVarDir, got.Workflow.Variables[WorkflowVarDir], wantDir)
	}
}

func TestExecRunAgent_UsesConfiguredScratchDirForPlan(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	scratchDir := t.TempDir()
	tasks.Put(TaskInfo{ID: "t1", Status: "plan-needed", AgentMode: "headless"})
	step := &Step{
		ID:   "plan",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:          "plan",
			Prompt:        "plan",
			NeedsWorktree: true,
			Dir:           scratchDir,
		},
	}
	wfExec := &Execution{
		WorkflowID: "plan-scratch",
		State:      ExecRunning,
		Variables: map[string]string{
			WorkflowVarDir: filepath.Join(os.TempDir(), "sybra-test-t1"),
		},
	}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	if len(agents.calls) != 1 {
		t.Fatalf("StartAgent calls = %d, want 1", len(agents.calls))
	}
	if agents.calls[0].Dir != scratchDir {
		t.Fatalf("StartAgent dir = %q, want configured scratch dir %q", agents.calls[0].Dir, scratchDir)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow.Variables[WorkflowVarDir] != scratchDir {
		t.Fatalf("%s = %q, want configured scratch dir %q", WorkflowVarDir, got.Workflow.Variables[WorkflowVarDir], scratchDir)
	}
}

func TestExecRunAgent_ABTestingOverridesProviderModel(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{{
				ID: "codex-gpt", Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "high", Weight: 1,
			}},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Model:  "sonnet",
			Prompt: "test prompt",
		},
	}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Provider != "codex" || call.Model != "gpt-5.5" {
		t.Fatalf("provider/model = %q/%q, want codex/gpt-5.5", call.Provider, call.Model)
	}
	if call.Assignment.ExperimentID != "exp" || call.Assignment.VariantID != "codex-gpt" || call.Assignment.ReasoningEffort != "high" {
		t.Fatalf("assignment = %+v", call.Assignment)
	}
	if call.Assignment.RoutingReason != "ab" {
		t.Fatalf("assignment routing reason = %q, want ab", call.Assignment.RoutingReason)
	}
}

func TestExecRunAgent_DefaultProviderPathWinsWhenABTestingOmitted(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetABTestingConfig(abtest.DefaultConfig())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Model:  "sonnet",
			Prompt: "test prompt",
		},
	}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Provider != "" || call.Model != "sonnet" {
		t.Fatalf("provider/model = %q/%q, want empty/sonnet so manager default-provider routing stays in control", call.Provider, call.Model)
	}
	if call.Assignment.ExperimentID != "" || call.Assignment.VariantID != "" {
		t.Fatalf("assignment = %+v, want no A/B assignment when ab_testing.enabled is omitted", call.Assignment)
	}
	if call.Assignment.RoutingReason != "" {
		t.Fatalf("assignment routing reason = %q, want empty", call.Assignment.RoutingReason)
	}
}

func TestExecRunAgentVariantPrompt(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "prompt-exp",
		Kind:           "prompt",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Subject:        &abtest.Subject{StepID: "implement"},
		Variants: []abtest.Variant{{
			ID: "copy-v2", Provider: "claude", Model: "sonnet", Weight: 1,
			PromptTransform: &abtest.PromptTransform{Op: "template", Text: "variant {{.Task.ID}} {{.Step.ID}}"},
		}},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "control {{.Task.ID}}"}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Prompt != "variant t1 implement" {
		t.Fatalf("Prompt = %q", call.Prompt)
	}
	if call.Assignment.PromptTransform == nil || call.Assignment.PromptTransform.Op != "template" {
		t.Fatalf("assignment prompt transform = %+v", call.Assignment.PromptTransform)
	}
}

// TestExecRunAgent_EvalGateBlocksFailingDigestedVariant wires a real
// prompteval.Gate (not a nil predicate) into the engine and proves a stored
// FAIL verdict for a digested variant keeps it out of online A/B enrollment
// on the production selectABVariant path — closing the gap where
// abtest.SelectEligibleWithEval existed but nothing in the dispatch hot path
// called it.
func TestExecRunAgent_EvalGateBlocksFailingDigestedVariant(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	evalStore := prompteval.New(t.TempDir())
	failingDigest := prompteval.Digest([]byte("failing prompt"))
	if err := evalStore.Write(prompteval.VariantVerdict{
		VariantID: "failing-variant",
		Digest:    failingDigest,
		Status:    prompteval.StatusFail,
		Runner:    "native",
	}); err != nil {
		t.Fatalf("Write verdict: %v", err)
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetEvalGate(prompteval.NewGate(evalStore, config.OfflineEvalConfig{}))
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "gated-exp",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Variants: []abtest.Variant{
			{ID: "failing-variant", Provider: "codex", Model: "gpt-5.5", Digest: failingDigest, Weight: 1},
			{ID: "clean-variant", Provider: "claude", Model: "sonnet", Weight: 1},
		},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "test prompt"}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Assignment.VariantID != "clean-variant" {
		t.Fatalf("assignment = %+v, want clean-variant (failing-variant must be excluded by the eval gate)", call.Assignment)
	}
}

func TestExecRunAgentSkillAlias(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "skill-exp",
		Kind:           "skill",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Subject:        &abtest.Subject{StepID: "implement", SkillName: "sybra-test"},
		Variants: []abtest.Variant{{
			ID: "skill-v2", Provider: "codex", Model: "gpt-5.5", Weight: 1,
			SkillAliases: map[string]string{"sybra-test": "sybra-test-v2"},
		}},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "Run /sybra-test, not /tmp/sybra-test.md."}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Prompt != "Run /sybra-test-v2, not /tmp/sybra-test.md." {
		t.Fatalf("Prompt = %q", call.Prompt)
	}
	if got := call.Assignment.SkillAliases["sybra-test"]; got != "sybra-test-v2" {
		t.Fatalf("assignment alias = %q", got)
	}
}

func TestExecRunAgentVariantSubjectMismatchDoesNotApplyPayload(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	tests := []struct {
		name    string
		subject *abtest.Subject
		step    Step
		wfID    string
	}{
		{
			name:    "step",
			subject: &abtest.Subject{StepID: "implement", SkillName: "sybra-test"},
			step:    Step{ID: "evaluate", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "control /sybra-test"}},
			wfID:    "test-simple",
		},
		{
			name:    "workflow",
			subject: &abtest.Subject{WorkflowID: "target-workflow", SkillName: "sybra-test"},
			step:    Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "control /sybra-test"}},
			wfID:    "other-workflow",
		},
		{
			name:    "skill",
			subject: &abtest.Subject{StepID: "implement", SkillName: "sybra-test"},
			step:    Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "control /other-skill"}},
			wfID:    "test-simple",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			enabled := true
			engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
				ID:             "subject-exp",
				Kind:           "skill",
				AssignmentUnit: "stage",
				Roles:          []string{"implementation"},
				Subject:        tt.subject,
				Variants: []abtest.Variant{{
					ID: "variant", Provider: "claude", Model: "sonnet", Weight: 1,
					PromptTransform: &abtest.PromptTransform{Op: "prepend", Text: "variant: "},
					SkillAliases:    map[string]string{"sybra-test": "sybra-test-v2"},
				}},
			}}})
			tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
			wfExec := &Execution{WorkflowID: tt.wfID, State: ExecRunning, Variables: map[string]string{}}
			ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: tt.step, Vars: wfExec.Variables}

			if err := engine.execRunAgent("t1", &tt.step, wfExec, ctx); err != nil {
				t.Fatal(err)
			}
			call := agents.LastCall()
			if call.Assignment.ExperimentID != "" {
				t.Fatalf("assignment applied despite subject mismatch: %+v", call.Assignment)
			}
			if call.Prompt != tt.step.Config.Prompt {
				t.Fatalf("prompt = %q, want %q", call.Prompt, tt.step.Config.Prompt)
			}
		})
	}
}

func TestExecRunAgentReuseAgentSkillAlias(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "skill-exp",
		Kind:           "skill",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Subject:        &abtest.Subject{StepID: "followup", SkillName: "sybra-test"},
		Variants: []abtest.Variant{{
			ID: "skill-v2", Provider: "claude", Model: "sonnet", Weight: 1,
			SkillAliases: map[string]string{"sybra-test": "sybra-test-v2"},
		}},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if _, _, _, err := agents.StartAgent("t1", "implementation", "headless", "sonnet", "claude", "initial", "", nil, false, false, "", "", AgentAssignment{}); err != nil {
		t.Fatal(err)
	}
	step := &Step{ID: "followup", Type: StepRunAgent, Config: StepConfig{Role: "implementation", ReuseAgent: true, Prompt: "Run /sybra-test"}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	sent := agents.SentPrompts()
	if len(sent) != 1 {
		t.Fatalf("SendPrompt count = %d, want 1", len(sent))
	}
	if sent[0].Message != "Run /sybra-test-v2" {
		t.Fatalf("sent prompt = %q", sent[0].Message)
	}
}

func TestSelectABVariantPropagatesExperimentKind(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "prompt-exp",
			Kind:           "prompt",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Subject:        &abtest.Subject{StepID: "implement"},
			Variants: []abtest.Variant{{
				ID: "copy-v2", Provider: "claude", Model: "sonnet", Weight: 1,
			}},
		}},
	})

	assignment, ok, err := engine.selectABVariant(abtest.SelectionContext{
		TaskID:     "t1",
		WorkflowID: "test-simple",
		Role:       "implementation",
		StepID:     "implement",
		Prompt:     "Run /sybra-test",
	})
	if err != nil || !ok {
		t.Fatalf("selectABVariant ok=%v err=%v", ok, err)
	}
	if assignment.Kind != "prompt" {
		t.Fatalf("Kind = %q, want prompt", assignment.Kind)
	}
}

func TestExecRunAgent_ABTestingSkipsRateLimitedProvider(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimitedFor("codex", true)
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "codex-gpt", Provider: "codex", Model: "gpt-5.5", Weight: 1},
				{ID: "claude-opus", Provider: "claude", Model: "opus", Weight: 1},
			},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Prompt: "test prompt",
		},
	}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Provider == "codex" {
		t.Fatalf("rate-limited provider was selected: %+v", call)
	}
	if call.Provider != "claude" || call.Assignment.VariantID != "claude-opus" {
		t.Fatalf("provider/assignment = %q/%q, want claude/claude-opus", call.Provider, call.Assignment.VariantID)
	}
}

func TestExecRunAgent_ABTestingSkipsConfigDisabledProvider(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	// copilot's CLI is on PATH and it isn't rate-limited, but it is
	// administratively disabled via config — the health gate reports it
	// unhealthy. selectABVariant must not deterministically wedge every
	// task on this step onto a provider that will always fail to start.
	agents.SetProviderUnhealthy("copilot", true)
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "copilot-variant", Provider: "copilot", Model: "gpt-5.5", Weight: 1},
				{ID: "claude-opus", Provider: "claude", Model: "opus", Weight: 1},
			},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Prompt: "test prompt",
		},
	}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	call := agents.LastCall()
	if call.Provider == "copilot" {
		t.Fatalf("config-disabled provider was selected: %+v", call)
	}
	if call.Provider != "claude" || call.Assignment.VariantID != "claude-opus" {
		t.Fatalf("provider/assignment = %q/%q, want claude/claude-opus", call.Provider, call.Assignment.VariantID)
	}
}

// TestExecRunAgent_ProviderDemotionEmitsThrottledSignal proves selection-time
// provider filtering (here: rate limiting) that changes the A/B outcome
// surfaces a first-class demotion signal — wanted/selected/reason — logged at
// Error on first occurrence and throttled to Debug on identical repeats, so a
// sustained rate limit does not flood the log with duplicate errors.
func TestExecRunAgent_ProviderDemotionEmitsThrottledSignal(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetProviderRateLimitedFor("codex", true)
	engine := NewTestEngine(store, tasks, agents, logger)
	enabled := true
	// codex carries almost all the weight so the unfiltered ("wanted")
	// selection is codex with overwhelming probability for any task ID,
	// making the test deterministic without hand-computing the hash.
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "codex-variant", Provider: "codex", Model: "gpt-5.5", Weight: 100},
				{ID: "claude-variant", Provider: "claude", Model: "opus", Weight: 1},
			},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "test prompt"}}

	runOnce := func(taskID string) {
		wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
		ctx := TemplateContext{Task: TaskInfo{ID: taskID}, Step: *step, Vars: wfExec.Variables}
		if err := engine.execRunAgent(taskID, step, wfExec, ctx); err != nil {
			t.Fatal(err)
		}
	}
	runOnce("t1")
	call := agents.LastCall()
	if call.Provider != "claude" {
		t.Fatalf("provider = %q, want claude (codex is rate-limited)", call.Provider)
	}

	var demotions []slog.Record
	for _, r := range records {
		if r.Message == "workflow.ab.provider_demoted" {
			demotions = append(demotions, r)
		}
	}
	if len(demotions) != 1 {
		t.Fatalf("got %d demotion records after first run, want 1: %+v", len(demotions), demotions)
	}
	first := demotions[0]
	if first.Level != slog.LevelError {
		t.Fatalf("first demotion level = %v, want Error", first.Level)
	}
	if got := recordAttr(first, "wanted_provider"); got != "codex" {
		t.Fatalf("wanted_provider = %q, want codex", got)
	}
	if got := recordAttr(first, "selected_provider"); got != "claude" {
		t.Fatalf("selected_provider = %q, want claude", got)
	}
	if got := recordAttr(first, "reason"); got != "rate_limited" {
		t.Fatalf("reason = %q, want rate_limited", got)
	}
	if got := recordAttr(first, "task_id"); got != "t1" {
		t.Fatalf("task_id = %q, want t1", got)
	}

	// A second identical demotion for a different task must still be
	// throttled to Debug — otherwise a sustained outage floods the log with
	// one Error per dispatch across the fleet.
	records = nil
	tasks.Put(TaskInfo{ID: "t2", Status: "todo", AgentMode: "headless"})
	runOnce("t2")
	var second []slog.Record
	for _, r := range records {
		if r.Message == "workflow.ab.provider_demoted" {
			second = append(second, r)
		}
	}
	if len(second) != 1 {
		t.Fatalf("got %d demotion records after second run, want 1: %+v", len(second), second)
	}
	if second[0].Level != slog.LevelDebug {
		t.Fatalf("repeat demotion level = %v, want Debug (throttled)", second[0].Level)
	}
	if got := recordAttr(second[0], "task_id"); got != "t2" {
		t.Fatalf("task_id = %q, want t2 on throttled cross-task repeat", got)
	}
}

// TestExecRunAgent_ProviderShutoutEmitsSignal proves the single-provider
// fallback is observable: when provider filtering excludes *every* variant of
// an experiment (all variants share one unhealthy provider), the experiment
// degrades silently to non-A/B dispatch — but that total shutout emits a
// distinct throttled signal so an operator can tell it apart from A/B being
// disabled or no experiment matching the role.
func TestExecRunAgent_ProviderShutoutEmitsSignal(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	var records []slog.Record
	logger := slog.New(&demotionRecordHandler{records: &records})

	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	// Every variant is on codex, and codex is rate-limited — provider
	// filtering zeroes out the whole experiment, forcing the fallback.
	agents.SetProviderRateLimitedFor("codex", true)
	engine := NewTestEngine(store, tasks, agents, logger)
	enabled := true
	engine.SetABTestingConfig(abtest.Config{
		Enabled: &enabled,
		Experiments: []abtest.Experiment{{
			ID:             "exp",
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "codex-a", Provider: "codex", Model: "gpt-5.5", Weight: 1},
				{ID: "codex-b", Provider: "codex", Model: "gpt-5.5-mini", Weight: 1},
			},
		}},
	})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	step := &Step{ID: "implement", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Prompt: "test prompt"}}
	wfExec := &Execution{WorkflowID: "test-simple", State: ExecRunning, Variables: make(map[string]string)}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	// Must not error the whole dispatch — falls back to normal selection.
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	var shutouts []slog.Record
	for _, r := range records {
		if r.Message == "workflow.ab.provider_shutout" {
			shutouts = append(shutouts, r)
		}
	}
	if len(shutouts) != 1 {
		t.Fatalf("got %d shutout records, want 1: %+v", len(shutouts), shutouts)
	}
	first := shutouts[0]
	if first.Level != slog.LevelError {
		t.Fatalf("shutout level = %v, want Error", first.Level)
	}
	if got := recordAttr(first, "wanted_provider"); got != "codex" {
		t.Fatalf("wanted_provider = %q, want codex", got)
	}
	if got := recordAttr(first, "reason"); got != "rate_limited" {
		t.Fatalf("reason = %q, want rate_limited", got)
	}
	if got := recordAttr(first, "experiment_id"); got != "exp" {
		t.Fatalf("experiment_id = %q, want exp", got)
	}
}

func TestAgentModeTemplate(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}

	// Advance past triage to implement step.
	agents.SimulateComplete("t1")
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "triage", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	// Implement step should have mode resolved from template.
	if agents.LastCall().Mode != "headless" {
		t.Fatalf("expected headless mode from template, got %q", agents.LastCall().Mode)
	}
}

// TestExecRunAgent_ConsumesSupervisorSteer verifies the workflow half of a
// watchdog headless nudge: when a step is (re-)dispatched and a steer is
// pending, execRunAgent prepends the correction to the agent's prompt and the
// steer is consumed exactly once. This is the path ResumeStalled drives when it
// re-runs a stalled run_agent step — the case a prior design missed entirely.
func TestExecRunAgent_ConsumesSupervisorSteer(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	tasks.SetSteer("t1", "stop retrying the failing command")

	step := &Step{
		ID:     "implement",
		Type:   StepRunAgent,
		Config: StepConfig{Role: "implementation", Mode: "headless", Prompt: "do the work"},
	}
	wfExec := &Execution{WorkflowID: "test-simple", CurrentStep: "implement", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	got := agents.LastCall().Prompt
	want := "Supervisor course-correction: stop retrying the failing command\n\ndo the work"
	if got != want {
		t.Fatalf("dispatched prompt = %q, want %q", got, want)
	}

	// One-shot: a second dispatch (steer consumed) carries only the step prompt.
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	if got := agents.LastCall().Prompt; got != "do the work" {
		t.Fatalf("second dispatch prompt = %q, want unsteered (steer already consumed)", got)
	}
}

func TestExecRunAgent_ResourcePressureParksWithoutConsumingSteer(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", AgentMode: "headless"})
	tasks.SetSteer("t1", "keep this steer")
	agents.SetAdmitDispatch("disk free 1.0% below minimum 5.0%")

	step := &Step{
		ID:     "implement",
		Type:   StepRunAgent,
		Config: StepConfig{Role: "implementation", Mode: "headless", Prompt: "do the work"},
	}
	wfExec := &Execution{WorkflowID: "test-simple", CurrentStep: "implement", State: ExecRunning, Variables: map[string]string{}}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1", Status: "in-progress"}, Step: *step, Vars: wfExec.Variables}

	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent call count = %d, want 0", got)
	}
	if wfExec.State != ExecWaiting {
		t.Fatalf("workflow state = %s, want waiting", wfExec.State)
	}
	if got := tasks.Reason("t1"); got != "work paused: machine under resource pressure — disk free 1.0% below minimum 5.0%" {
		t.Fatalf("status_reason = %q", got)
	}

	agents.SetAdmitDispatch("")
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}
	got := agents.LastCall().Prompt
	want := "Supervisor course-correction: keep this steer\n\ndo the work"
	if got != want {
		t.Fatalf("prompt after pressure clears = %q, want %q", got, want)
	}
}

// TestExecRunAgent_TracksSpawnedStep verifies that execRunAgent persists the
// workflow agent route so HandleAgentComplete can route completions back to
// the right step. Without this mapping, a delayed completion from a duplicate
// agent would be credited to whatever CurrentStep happens to be at the moment
// — the exact bug that corrupted review_plan.
func TestExecRunAgent_TracksSpawnedStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "planning", AgentMode: "interactive"})

	step := &Step{
		ID:   "plan",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "plan",
			Mode:   "interactive",
			Prompt: "p",
		},
	}
	wfExec := &Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "plan",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}
	ctx := TemplateContext{Task: TaskInfo{ID: "t1"}, Step: *step, Vars: wfExec.Variables}
	if err := engine.execRunAgent("t1", step, wfExec, ctx); err != nil {
		t.Fatal(err)
	}

	agentID := agents.LastID()
	if gotStep, tracked := lookupWorkflowAgentRoute(t, engine, "t1", agentID); !tracked {
		t.Fatalf("agentRoutes missing entry for agent %s", agentID)
	} else if gotStep != "plan" {
		t.Errorf("agentRoutes[%s] = %q, want plan", agentID, gotStep)
	}

	// Completing the agent must clear its mapping so the map doesn't grow
	// unbounded across long-lived sessions.
	tasks.SetStatus("t1", "plan-review")
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: agentID, Result: "done", Success: true})

	if _, stillThere := lookupWorkflowAgentRoute(t, engine, "t1", agentID); stillThere {
		t.Errorf("agentRoutes still has %s after completion — mapping leaked", agentID)
	}
}

// TestExecRunAgent_DispatchInFlightWaits asserts a run_agent step whose
// StartAgent loses the per-task dispatch claim parks the workflow in
// ExecWaiting (the claim holder's agent will drive it) rather than failing the
// step and routing toward human-required.
func TestExecRunAgent_DispatchInFlightWaits(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	agents := newMockAgents()
	agents.SetFailSpawn(ErrDispatchInFlight)
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatalf("StartWorkflow returned error, want nil (dispatch-in-flight is benign): %v", err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Workflow == nil || ti.Workflow.State != ExecWaiting {
		t.Fatalf("workflow state = %v, want ExecWaiting", ti.Workflow)
	}
	if ti.Status == "human-required" {
		t.Errorf("dispatch-in-flight must not flip task to human-required")
	}
	if agents.CallCount() != 0 {
		t.Errorf("no agent should have been recorded as started, got %d calls", agents.CallCount())
	}
}

func TestExecRunAgent_PanicClearsDispatchingClaim(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	agents := &panicStartAgents{mockAgents: newMockAgents()}
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = engine.StartWorkflow("t1", "test-simple")
	}()

	if recovered == nil {
		t.Fatal("expected StartWorkflow to panic when StartAgent panics")
		return
	}
	if engine.hasTrackedAgentForTaskStep("t1", "triage") {
		t.Fatal("pending step start leaked after panic")
	}
}

func TestExecRunAgent_PreStartFailureReleasesClaimedEffect(t *testing.T) {
	tasks := newMemTasks()
	engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	step := &Step{
		ID:   "implement",
		Type: StepRunAgent,
		Config: StepConfig{
			Role:   "implementation",
			Prompt: "{{",
			Mode:   "headless",
		},
	}
	wf := &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}
	ti := TaskInfo{
		ID:         "t1",
		Status:     "in-progress",
		AgentMode:  "headless",
		Generation: 1,
		Workflow:   wf.Clone(),
	}
	tasks.Put(ti)

	claim := engine.effectClaimForStep(ti, step, effectPosStepAction)
	if _, err := tasks.ClaimWorkflowEffect("t1", claim); err != nil {
		t.Fatalf("ClaimWorkflowEffect: %v", err)
	}

	if err := engine.execRunAgent("t1", step, wf.Clone(), TemplateContext{
		Task:     ti,
		Step:     *step,
		Vars:     wf.Variables,
		Workflow: wf,
	}, claim.EffectID); err == nil {
		t.Fatal("execRunAgent error = nil, want prompt render failure")
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Workflow == nil {
		t.Fatal("workflow = nil, want persisted workflow")
	}
	if len(got.Workflow.EffectLog) != 0 {
		t.Fatalf("EffectLog = %+v, want claimed effect released on pre-start failure", got.Workflow.EffectLog)
	}
	if _, err := tasks.ClaimWorkflowEffect("t1", claim); err != nil {
		t.Fatalf("ClaimWorkflowEffect after release: %v, want success", err)
	}
}

// TestExecRunAgent_RealSpawnErrorPropagates is the contrast to the test above:
// a genuine (non-dispatch-in-flight) spawn error must still surface.
func TestExecRunAgent_RealSpawnErrorPropagates(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	agents := newMockAgents()
	agents.SetFailSpawn(errors.New("worktree boom"))
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	if err := engine.StartWorkflow("t1", "test-simple"); err == nil {
		t.Fatal("StartWorkflow should propagate a real spawn error")
	}
}
