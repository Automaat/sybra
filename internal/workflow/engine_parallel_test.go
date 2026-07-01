package workflow

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/abtest"
)

// TestParallelValidation_Rejects covers the model.Validate guard rails for
// `parallel` steps: too few children, non-run_agent children, nested
// parallels, duplicate step IDs across the whole definition.
func TestParallelValidation_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		def    Definition
		errSub string
	}{
		{
			name: "fewer than 2 children",
			def: Definition{
				ID: "x",
				Steps: []Step{{
					ID: "p", Type: StepParallel,
					Parallel: []Step{{ID: "a", Type: StepRunAgent}},
				}},
			},
			errSub: "at least 2 children",
		},
		{
			name: "non-run_agent child",
			def: Definition{
				ID: "x",
				Steps: []Step{{
					ID: "p", Type: StepParallel,
					Parallel: []Step{
						{ID: "a", Type: StepRunAgent},
						{ID: "b", Type: StepSetStatus},
					},
				}},
			},
			errSub: "only \"run_agent\" allowed",
		},
		{
			name: "nested parallel",
			def: Definition{
				ID: "x",
				Steps: []Step{{
					ID: "p", Type: StepParallel,
					Parallel: []Step{
						{ID: "a", Type: StepRunAgent},
						{ID: "b", Type: StepRunAgent, Parallel: []Step{
							{ID: "z", Type: StepRunAgent},
						}},
					},
				}},
			},
			errSub: "nests another parallel",
		},
		{
			name: "duplicate child id",
			def: Definition{
				ID: "x",
				Steps: []Step{
					{ID: "p", Type: StepParallel, Parallel: []Step{
						{ID: "a", Type: StepRunAgent},
						{ID: "a", Type: StepRunAgent},
					}},
				},
			},
			errSub: "already used elsewhere",
		},
		{
			name: "child id collides with sibling top-level",
			def: Definition{
				ID: "x",
				Steps: []Step{
					{ID: "first", Type: StepRunAgent},
					{ID: "p", Type: StepParallel, Parallel: []Step{
						{ID: "first", Type: StepRunAgent},
						{ID: "b", Type: StepRunAgent},
					}},
				},
			},
			errSub: "already used elsewhere",
		},
		{
			name: "parallel field on non-parallel step",
			def: Definition{
				ID: "x",
				Steps: []Step{{
					ID: "r", Type: StepRunAgent,
					Parallel: []Step{{ID: "a", Type: StepRunAgent}, {ID: "b", Type: StepRunAgent}},
				}},
			},
			errSub: "parallel children only allowed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.def.Validate()
			if err == nil {
				t.Fatalf("expected validation error containing %q, got nil", tc.errSub)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("err = %v\nwant substring %q", err, tc.errSub)
			}
		})
	}
}

// TestParallelValidation_AcceptsValid confirms a well-formed parallel block
// passes validation.
func TestParallelValidation_AcceptsValid(t *testing.T) {
	t.Parallel()
	def := Definition{
		ID: "x",
		Trigger: Trigger{
			On: "task.created",
		},
		Steps: []Step{
			{
				ID: "plan", Type: StepParallel,
				Parallel: []Step{
					{ID: "a", Type: StepRunAgent, Config: StepConfig{Role: "plan", Provider: "claude", Mode: "headless", Prompt: "x"}},
					{ID: "b", Type: StepRunAgent, Config: StepConfig{Role: "plan", Provider: "codex", Mode: "headless", Prompt: "x"}},
				},
				Next: []Transition{{GoTo: "converge"}},
			},
			{ID: "converge", Type: StepRunAgent, Config: StepConfig{Role: "plan", Mode: "headless", Prompt: "x"}},
		},
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	// StepByID must reach into Parallel children.
	if got := def.StepByID("a"); got == nil || got.Config.Provider != "claude" {
		t.Errorf("StepByID('a') did not reach into parallel children: %+v", got)
	}
	if got := def.StepByID("b"); got == nil || got.Config.Provider != "codex" {
		t.Errorf("StepByID('b') did not reach into parallel children: %+v", got)
	}
}

// TestParallel_DispatchesAllChildren verifies the parallel parent spawns
// every child agent (one StartAgent call per child) using each child's
// configured provider/role.
func TestParallel_DispatchesAllChildren(t *testing.T) {
	// Stub providerAvailable so the engine's fallback (which strips the
	// configured provider when the CLI isn't on PATH) doesn't run. Without
	// this, the test fails on CI runners that have neither claude nor
	// codex installed.
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	if err := engine.StartWorkflow("t1", "test-parallel"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	// Two children should have been spawned; converge should NOT have run yet.
	if got := agents.CallCount(); got != 2 {
		t.Fatalf("StartAgent calls = %d, want 2", got)
	}
	calls := agents.calls
	providers := []string{calls[0].Provider, calls[1].Provider}
	if !slices.Contains(providers, "claude") || !slices.Contains(providers, "codex") {
		t.Errorf("expected both providers spawned, got %v", providers)
	}

	wf := mustWorkflow(t, tasks, "t1")
	if wf.CurrentStep != "plan" {
		t.Errorf("CurrentStep = %q, want plan", wf.CurrentStep)
	}
	if rec := wf.ParallelInflight["plan"]; rec == nil {
		t.Fatalf("expected ParallelInflight[plan] populated")
	} else {
		if len(rec.Children) != 2 {
			t.Errorf("Children count = %d, want 2", len(rec.Children))
		}
		for id, c := range rec.Children {
			if c.Status != "pending" {
				t.Errorf("child %q status = %q, want pending", id, c.Status)
			}
			if c.AgentID == "" {
				t.Errorf("child %q has empty AgentID", id)
			}
		}
	}
}

func TestParallel_AppliesABAssignmentToAuthorChildren(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	def := Definition{
		ID:      "ab-parallel",
		Trigger: Trigger{On: "manual"},
		Steps: []Step{{
			ID:   "implement_both",
			Type: StepParallel,
			Parallel: []Step{
				{ID: "impl_a", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Mode: "headless", Prompt: "a"}},
				{ID: "impl_b", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Mode: "headless", Prompt: "b"}},
			},
		}},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("save workflow: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "exp",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Variants:       []abtest.Variant{{ID: "opus", Provider: "claude", Model: "opus", Weight: 1}},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	if err := engine.StartWorkflow("t1", "ab-parallel"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if got := agents.CallCount(); got != 2 {
		t.Fatalf("StartAgent calls = %d, want 2", got)
	}
	for _, c := range agents.calls {
		if c.Provider != "claude" || c.Model != "opus" || c.Assignment.ExperimentID != "exp" || c.Assignment.VariantID != "opus" {
			t.Fatalf("parallel child call missing AB assignment: %+v", c)
		}
	}
}

func TestParallel_AppliesPromptAndSkillVariantPayloads(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	def := Definition{
		ID:      "ab-parallel-payload",
		Trigger: Trigger{On: "manual"},
		Steps: []Step{{
			ID:   "implement_both",
			Type: StepParallel,
			Parallel: []Step{
				{ID: "impl_a", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Mode: "headless", Prompt: "control {{.Step.ID}} /sybra-test"}},
				{ID: "impl_b", Type: StepRunAgent, Config: StepConfig{Role: "implementation", Mode: "headless", Prompt: "control {{.Step.ID}} /sybra-test"}},
			},
		}},
	}
	if err := store.Save(def); err != nil {
		t.Fatalf("save workflow: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	enabled := true
	engine.SetABTestingConfig(abtest.Config{Enabled: &enabled, Experiments: []abtest.Experiment{{
		ID:             "payload-exp",
		Kind:           "compound",
		AssignmentUnit: "stage",
		Roles:          []string{"implementation"},
		Variants: []abtest.Variant{{
			ID: "payload", Provider: "claude", Model: "sonnet", Weight: 1,
			PromptTransform: &abtest.PromptTransform{Op: "prepend", Text: "variant {{.Step.ID}}: "},
			SkillAliases:    map[string]string{"sybra-test": "sybra-test-v2"},
		}},
	}}})
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	if err := engine.StartWorkflow("t1", "ab-parallel-payload"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if got := agents.CallCount(); got != 2 {
		t.Fatalf("StartAgent calls = %d, want 2", got)
	}
	for _, c := range agents.calls {
		want := "variant " + c.Assignment.AssignmentKey[strings.LastIndex(c.Assignment.AssignmentKey, "|")+1:] + ": control " + c.Assignment.AssignmentKey[strings.LastIndex(c.Assignment.AssignmentKey, "|")+1:] + " /sybra-test-v2"
		if c.Prompt != want {
			t.Fatalf("parallel prompt = %q, want %q", c.Prompt, want)
		}
		if c.Assignment.PromptTransform == nil || c.Assignment.SkillAliases["sybra-test"] != "sybra-test-v2" {
			t.Fatalf("parallel assignment missing payload: %+v", c.Assignment)
		}
	}
}

// TestParallel_AllCompleteAdvancesParent verifies that once every child
// reports status=completed the parent step is recorded with status=completed
// and the next step is dispatched.
func TestParallel_AllCompleteAdvancesParent(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	if err := engine.StartWorkflow("t1", "test-parallel"); err != nil {
		t.Fatal(err)
	}

	// Complete plan_a first, then plan_b. After plan_b the parent should
	// advance to converge (a third agent spawn).
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan_a", Status: "completed", AgentID: "agent-1", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	wf := mustWorkflow(t, tasks, "t1")
	if rec := wf.ParallelInflight["plan"]; rec == nil {
		t.Fatal("ParallelInflight[plan] cleared too early")
	}
	if got := agents.CallCount(); got != 2 {
		t.Errorf("converge spawned before all children done: agent calls=%d", got)
	}

	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan_b", Status: "completed", AgentID: "agent-2", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	wf = mustWorkflow(t, tasks, "t1")
	if _, still := wf.ParallelInflight["plan"]; still {
		t.Errorf("ParallelInflight[plan] should be cleared after all children done")
	}
	rec := findStepRecord(wf, "plan")
	if rec == nil {
		t.Fatal("parent step record missing")
	}
	if rec.Status != "completed" {
		t.Errorf("parent status = %q, want completed", rec.Status)
	}
	if got := agents.CallCount(); got != 3 {
		t.Errorf("converge should have been spawned: agent calls=%d, want 3", got)
	}
	if last := agents.LastCall(); last.Role != "plan" || last.Model != "sonnet" {
		t.Errorf("converge spawn = %+v, want role=plan model=sonnet", last)
	}
}

// TestParallel_ChildFailRetryThenSucceed verifies per-child retry: a failed
// child gets re-spawned within max_retries, and a subsequent success counts
// toward parent completion.
func TestParallel_ChildFailRetryThenSucceed(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	if err := engine.StartWorkflow("t1", "test-parallel"); err != nil {
		t.Fatal(err)
	}
	startCalls := agents.CallCount()

	// plan_b fails first time. test-parallel.yaml gives plan_b max_retries=1.
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan_b", Status: "failed", AgentID: "agent-2", Output: "boom"}); err != nil {
		t.Fatal(err)
	}
	if got := agents.CallCount(); got != startCalls+1 {
		t.Errorf("expected one retry spawn, got %d new calls", got-startCalls)
	}
	wf := mustWorkflow(t, tasks, "t1")
	rec := wf.ParallelInflight["plan"]
	if rec == nil {
		t.Fatal("ParallelInflight cleared before retry")
	}
	got := rec.Children["plan_b"]
	if got == nil {
		t.Fatal("plan_b child missing after retry")
	}
	if got.Retries != 1 || got.Status != "pending" {
		t.Errorf("plan_b after retry: %+v (want retries=1 status=pending)", got)
	}

	// Now the retry succeeds along with plan_a.
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan_a", Status: "completed", AgentID: "agent-1"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan_b", Status: "completed", AgentID: agents.LastID()}); err != nil {
		t.Fatal(err)
	}
	wf = mustWorkflow(t, tasks, "t1")
	if _, still := wf.ParallelInflight["plan"]; still {
		t.Errorf("ParallelInflight should be cleared after all children done")
	}
	if r := findStepRecord(wf, "plan"); r == nil || r.Status != "completed" {
		t.Errorf("parent record after retry-success: %+v (want completed)", r)
	}
}

// TestParallel_ChildFailExhaustedFailsParent verifies that once a child has
// burned its retry budget on failures, the parent step is recorded as
// failed and the workflow advances accordingly.
func TestParallel_ChildFailExhaustedFailsParent(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	if err := engine.StartWorkflow("t1", "test-parallel"); err != nil {
		t.Fatal(err)
	}

	// plan_b: fail (retry), fail (exhausted).
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan_b", Status: "failed", AgentID: "agent-2"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan_b", Status: "failed", AgentID: agents.LastID()}); err != nil {
		t.Fatal(err)
	}
	// plan_a finishes cleanly.
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "plan_a", Status: "completed", AgentID: "agent-1"}); err != nil {
		t.Fatal(err)
	}
	wf := mustWorkflow(t, tasks, "t1")
	if _, still := wf.ParallelInflight["plan"]; still {
		t.Errorf("ParallelInflight should be cleared after all children done")
	}
	rec := findStepRecord(wf, "plan")
	if rec == nil || rec.Status != "failed" {
		t.Errorf("parent record after exhausted retries: %+v (want failed)", rec)
	}
}

// TestParallel_AllSpawnsFail_AdvancesParent regresses a deadlock: when every
// child in a parallel block fails at spawn time (e.g. missing project), no
// agent ever runs, so HandleAgentComplete never fires. The engine must
// advance the parent synchronously to status=failed instead of leaving the
// workflow stuck in state=waiting forever.
func TestParallel_AllSpawnsFail_AdvancesParent(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel-terminal.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	agents.SetFailSpawn(errors.New("project not registered locally"))

	if err := engine.StartWorkflow("t1", "test-parallel-terminal"); err != nil {
		t.Fatalf("StartWorkflow returned err = %v; engine must surface a failed parent record instead of bubbling the spawn error", err)
	}

	wf := mustWorkflow(t, tasks, "t1")
	if _, still := wf.ParallelInflight["plan"]; still {
		t.Errorf("ParallelInflight[plan] should be cleared after all spawns failed")
	}
	rec := findStepRecord(wf, "plan")
	if rec == nil {
		t.Fatal("parent step record missing — workflow deadlocked in state=waiting")
	}
	if rec.Status != "failed" {
		t.Errorf("parent status = %q, want failed", rec.Status)
	}
	if wf.State == ExecWaiting {
		t.Errorf("workflow state = %q, want terminal (not waiting)", wf.State)
	}
	if got := agents.CallCount(); got != 0 {
		t.Errorf("recorded successful StartAgent calls = %d, want 0 (spawn was armed to fail)", got)
	}
}

// TestParallel_PlanDraftSidecarKeyedByStepID verifies the auto-keying
// convention: a child whose YAML import_sidecar.kind is the bare
// "plan_draft" gets stored under `plan_draft.<child step ID>`. This is what
// lets workflows fan out to N planners without enumerating each kind.
func TestParallel_PlanDraftSidecarKeyedByStepID(t *testing.T) {
	store := newTestStoreWith(t, "test-parallel.yaml")
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})

	if err := engine.StartWorkflow("t1", "test-parallel"); err != nil {
		t.Fatal(err)
	}

	// Set up sidecar files for both children. importSidecarIfConfigured
	// reads from disk; write the per-child draft files using the path
	// pattern from test-parallel.yaml.
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir) // not strictly necessary; paths are absolute /tmp
	for _, child := range []string{"plan_a", "plan_b"} {
		path := "/tmp/sybra-plan-draft-" + child + "-t1.md"
		if err := writeFile(path, "draft for "+child); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
		t.Cleanup(func() { _ = removeFile(path) })
	}

	// Trigger the importSidecarIfConfigured path via HandleAgentComplete,
	// which is the production code path.
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-1", Success: true, Result: "ok", Provider: "claude"})
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: "agent-2", Success: true, Result: "ok", Provider: "codex"})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ti.PlanDrafts["plan_a"]; got != "draft for plan_a" {
		t.Errorf("plan_a draft not stored under plan_draft.plan_a: got %q", got)
	}
	if got := ti.PlanDrafts["plan_b"]; got != "draft for plan_b" {
		t.Errorf("plan_b draft not stored under plan_draft.plan_b: got %q", got)
	}
}

// --- helpers ---

func mustWorkflow(t *testing.T, tasks *memTasks, id string) *Execution {
	t.Helper()
	ti, err := tasks.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if ti.Workflow == nil {
		t.Fatalf("task %s has no workflow", id)
	}
	return ti.Workflow
}

func findStepRecord(wf *Execution, stepID string) *StepRecord {
	for i := range slices.Backward(wf.StepHistory) {
		if wf.StepHistory[i].StepID == stepID {
			return &wf.StepHistory[i]
		}
	}
	return nil
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
func removeFile(path string) error {
	return os.Remove(path)
}
