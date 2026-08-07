package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importSidecarFixture wires a Store with the test-import-sidecar workflow,
// a memTasks holding a task whose Workflow.Variables points sidecar_path at
// fixturePath, and a fresh Engine. Returns engine + tasks so the test can
// drive importSidecarIfConfigured directly without needing a full
// HandleAgentComplete plumbing.
func importSidecarFixture(t *testing.T, fixturePath string) (*Engine, *memTasks) {
	t.Helper()
	store := newTestStoreWith(t, "test-import-sidecar.yaml")
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-import-sidecar",
			CurrentStep: "review",
			Variables:   map[string]string{"sidecar_path": fixturePath},
		},
	})
	agents := newMockAgents()
	return NewTestEngine(store, tasks, agents, discardLogger()), tasks
}

func TestImportSidecar_WritesContentFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review.md")
	body := "# Review\n\nNo findings.\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	engine, tasks := importSidecarFixture(t, path)

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info)

	got, _ := tasks.GetTask("t1")
	if got.CodeReview != body {
		t.Errorf("CodeReview = %q, want %q", got.CodeReview, body)
	}
}

func TestImportSidecar_MissingFileIsLoggedNotFatal(t *testing.T) {
	engine, tasks := importSidecarFixture(t, filepath.Join(t.TempDir(), "missing.md"))

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info)

	got, _ := tasks.GetTask("t1")
	if got.CodeReview != "" {
		t.Errorf("CodeReview = %q, want empty (require_sidecar should surface)", got.CodeReview)
	}
}

func TestImportSidecar_RequiredMissingFileFlipsHumanRequired(t *testing.T) {
	store := newTestStoreWith(t, "test-import-required-sidecar.yaml")
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Workflow: &Execution{
			WorkflowID:  "test-import-required-sidecar",
			CurrentStep: "plan",
			Variables:   map[string]string{"contract_path": filepath.Join(t.TempDir(), "missing.json")},
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "plan", info)

	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "required plan contract sidecar missing after step plan") {
		t.Fatalf("reason = %q, want required sidecar missing", reason)
	}
}

// TestImportSidecar_EmptyDirVarDistinguishedFromMissing proves the
// sybra#1495 diagnostic fix: when a sidecar path template references the
// reserved worktree-dir var (_dir) and that var is empty at render time, the
// escalation reason says so explicitly instead of the generic "missing" —
// so an investigator doesn't misread an engine-lost-worktree-dir bug as "the
// agent never wrote the review".
func TestImportSidecar_EmptyDirVarDistinguishedFromMissing(t *testing.T) {
	store := newTestStoreWith(t, "test-import-required-sidecar-dir.yaml")
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-import-required-sidecar-dir",
			CurrentStep: "review",
			Variables:   map[string]string{}, // _dir never set
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info)

	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "unresolved: worktree dir variable was empty at render time") {
		t.Fatalf("reason = %q, want empty-dir-var diagnostic, not generic missing", reason)
	}
	if strings.Contains(reason, "sidecar missing") {
		t.Fatalf("reason = %q, must not read as a plain missing sidecar", reason)
	}
}

func TestImportSidecar_EmptyDirVarRecoversViaWorktreeGetter(t *testing.T) {
	store := newTestStoreWith(t, "test-import-required-sidecar-dir.yaml")
	worktree := t.TempDir()
	body := `{"task_id":"t1"}`
	if err := os.WriteFile(filepath.Join(worktree, "review.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-import-required-sidecar-dir",
			CurrentStep: "review",
			Variables:   map[string]string{}, // lost during restart/reattach
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: worktree, ok: true})

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info)

	got, _ := tasks.GetTask("t1")
	if got.Status != "in-progress" {
		t.Fatalf("Status = %q, want in-progress", got.Status)
	}
	if got.PlanContract != body {
		t.Fatalf("PlanContract = %q, want recovered sidecar", got.PlanContract)
	}
	if got.Workflow == nil {
		t.Fatal("Workflow is nil, want persisted recovered worktree")
	}
	if got.Workflow.Variables[WorkflowVarDir] != worktree {
		t.Fatalf("%s = %q, want persisted recovered worktree", WorkflowVarDir, got.Workflow.Variables[WorkflowVarDir])
	}
	if reason := tasks.Reason("t1"); reason != "" {
		t.Fatalf("reason = %q, want no human-required escalation", reason)
	}
}

func TestImportSidecars_RecoveredDirMakesLaterMissingArtifactPlainMissing(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(Definition{
		ID:   "test-import-multiple-dir-sidecars",
		Name: "Test Import Multiple Dir Sidecars",
		Steps: []Step{{
			ID:   "plan",
			Type: StepRunAgent,
			Config: StepConfig{
				Role:   "plan",
				Mode:   "headless",
				Prompt: "plan",
				ImportSidecars: []ImportSidecar{
					{
						Kind:     "plan_research",
						From:     `{{getvar .Vars "_dir"}}/research.md`,
						Required: true,
					},
					{
						Kind:     "plan_contract",
						From:     `{{getvar .Vars "_dir"}}/missing-contract.json`,
						Required: true,
					},
				},
			},
		}},
	}); err != nil {
		t.Fatalf("save workflow: %v", err)
	}

	worktree := t.TempDir()
	research := "# Research\n"
	if err := os.WriteFile(filepath.Join(worktree, "research.md"), []byte(research), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Workflow: &Execution{
			WorkflowID:  "test-import-multiple-dir-sidecars",
			CurrentStep: "plan",
			Variables:   map[string]string{},
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: worktree, ok: true})

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "plan", info)

	got, _ := tasks.GetTask("t1")
	if got.PlanResearch != research {
		t.Fatalf("PlanResearch = %q, want recovered first sidecar", got.PlanResearch)
	}
	if got.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required for missing second sidecar", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "required plan contract sidecar missing after step plan") {
		t.Fatalf("reason = %q, want plain missing sidecar after recovered _dir", reason)
	}
	if strings.Contains(reason, "unresolved") {
		t.Fatalf("reason = %q, must not reuse stale empty-_dir snapshot after recovery", reason)
	}
}

// TestImportSidecar_MissingFileWithDirSetStillReportsMissing proves the new
// empty-_dir diagnostic in importOneSidecar doesn't shadow the ordinary
// "agent never wrote the file" case once _dir is genuinely populated.
func TestImportSidecar_MissingFileWithDirSetStillReportsMissing(t *testing.T) {
	store := newTestStoreWith(t, "test-import-required-sidecar-dir.yaml")
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-import-required-sidecar-dir",
			CurrentStep: "review",
			Variables:   map[string]string{"_dir": t.TempDir()},
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info)

	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "sidecar missing") {
		t.Fatalf("reason = %q, want plain missing sidecar", reason)
	}
	if strings.Contains(reason, "unresolved") {
		t.Fatalf("reason = %q, must not claim an unresolved dir var when _dir was set", reason)
	}
}

// TestImportSidecar_RecoversFromTaskWorktreeWhenDirVarEmpty proves the
// sybra#1988 fix: when _dir is lost at render time but the agent's sidecar
// really was written into the task's worktree, import recovers the worktree
// dir from task metadata (the same WorktreeGetter lookup verify_checks/
// tamper/codegen use) instead of escalating to human-required.
func TestImportSidecar_RecoversFromTaskWorktreeWhenDirVarEmpty(t *testing.T) {
	store := newTestStoreWith(t, "test-import-required-sidecar-dir.yaml")
	wt := t.TempDir()
	body := "# Review\n\nNo findings.\n"
	if err := os.WriteFile(filepath.Join(wt, "review.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-import-required-sidecar-dir",
			CurrentStep: "review",
			Variables:   map[string]string{}, // _dir never set
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info)

	got, _ := tasks.GetTask("t1")
	if got.Status == "human-required" {
		t.Fatalf("Status = %q, want task not escalated: reason=%q", got.Status, tasks.Reason("t1"))
	}
	if got.PlanContract != body {
		t.Errorf("PlanContract = %q, want %q", got.PlanContract, body)
	}
	if got.Workflow.Variables[WorkflowVarDir] != wt {
		t.Errorf("_dir = %q, want recovered worktree %q persisted for later steps", got.Workflow.Variables[WorkflowVarDir], wt)
	}
}

// TestImportSidecar_RecoveryStillEscalatesWhenFileAlsoMissingInWorktree
// proves the recovery path doesn't mask a genuine "agent never wrote the
// file" failure: even after resolving the real worktree dir, a missing file
// still flips the task to human-required with the original diagnostic.
func TestImportSidecar_RecoveryStillEscalatesWhenFileAlsoMissingInWorktree(t *testing.T) {
	store := newTestStoreWith(t, "test-import-required-sidecar-dir.yaml")
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-import-required-sidecar-dir",
			CurrentStep: "review",
			Variables:   map[string]string{}, // _dir never set
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true}) // no review.md written here

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info)

	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "unresolved: worktree dir variable was empty at render time") {
		t.Fatalf("reason = %q, want empty-dir-var diagnostic, not generic missing", reason)
	}
}

func TestImportSidecar_NonReservedDirTemplateStillReportsMissing(t *testing.T) {
	store := newTestStoreWith(t, "test-import-required-sidecar-nonreserved-dir.yaml")
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "in-progress",
		Workflow: &Execution{
			WorkflowID:  "test-import-required-sidecar-nonreserved-dir",
			CurrentStep: "review",
			Variables:   map[string]string{},
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info)

	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "sidecar missing") {
		t.Fatalf("reason = %q, want plain missing sidecar", reason)
	}
	if strings.Contains(reason, "unresolved: worktree dir variable was empty at render time") {
		t.Fatalf("reason = %q, must not claim reserved _dir was unresolved for a non-reserved path", reason)
	}
}

func TestImportSidecar_NoConfigIsNoop(t *testing.T) {
	store := newTestStore(t) // test-simple.yaml has no import_sidecar
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID: "t1",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "implement", info)

	got, _ := tasks.GetTask("t1")
	if got.CodeReview != "" || got.PlanCritique != "" {
		t.Errorf("sidecars touched without config: code=%q plan=%q", got.CodeReview, got.PlanCritique)
	}
}

func TestImportSidecar_NoWorkflowIsNoop(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1"}) // no Workflow attached
	engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info) // must not panic
}

func TestImportSidecar_UnknownKindLogsAndDoesNotWrite(t *testing.T) {
	store := newTestStoreWith(t, "test-import-sidecar-bad-kind.yaml")
	dir := t.TempDir()
	path := filepath.Join(dir, "review.md")
	if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID: "t1",
		Workflow: &Execution{
			WorkflowID:  "test-import-sidecar-bad-kind",
			CurrentStep: "review",
			Variables:   map[string]string{"sidecar_path": path},
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "review", info) // must not panic

	got, _ := tasks.GetTask("t1")
	if got.CodeReview != "" || got.PlanCritique != "" {
		t.Errorf("unknown kind silently wrote a sidecar: code=%q plan=%q", got.CodeReview, got.PlanCritique)
	}
}

func TestImportSidecars_WritesMultiplePlanningArtifacts(t *testing.T) {
	store := newTestStoreWith(t, "test-import-sidecars.yaml")
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	research := "# Research\n"
	decisions := "# Decisions\n"
	plan := "# Execution Plan\n"
	brief := "# Final Brief\n"
	contract := validPlanContract("t1")
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Workflow: &Execution{
			WorkflowID:  "test-import-sidecars",
			CurrentStep: "plan",
			Variables: map[string]string{
				"research_path":  write("research.md", research),
				"decisions_path": write("decisions.md", decisions),
				"plan_path":      write("plan.md", plan),
				"brief_path":     write("brief.md", brief),
				"contract_path":  write("contract.json", contract),
			},
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	info, _ := tasks.GetTask("t1")
	engine.importSidecarIfConfigured("t1", "plan", info)

	got, _ := tasks.GetTask("t1")
	if got.PlanResearch != research {
		t.Errorf("PlanResearch = %q, want %q", got.PlanResearch, research)
	}
	if got.PlanDecisions != decisions {
		t.Errorf("PlanDecisions = %q, want %q", got.PlanDecisions, decisions)
	}
	if got.Plan != plan {
		t.Errorf("Plan = %q, want %q", got.Plan, plan)
	}
	if got.PlanBrief != brief {
		t.Errorf("PlanBrief = %q, want %q", got.PlanBrief, brief)
	}
	if got.PlanContract != contract {
		t.Errorf("PlanContract = %q, want %q", got.PlanContract, contract)
	}
}

func newImportSidecarsFixture(t *testing.T, files map[string]string) (*Engine, *memTasks, string) {
	t.Helper()
	store := newTestStoreWith(t, "test-import-sidecars.yaml")
	dir := t.TempDir()
	vars := map[string]string{}
	for name, key := range map[string]string{
		"research.md":   "research_path",
		"decisions.md":  "decisions_path",
		"plan.md":       "plan_path",
		"brief.md":      "brief_path",
		"contract.json": "contract_path",
	} {
		vars[key] = filepath.Join(dir, name)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "planning",
		Workflow: &Execution{
			WorkflowID:  "test-import-sidecars",
			CurrentStep: "plan",
			Variables:   vars,
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	return engine, tasks, dir
}

func TestAdoptSidecarsFromFailedRun_AdoptsCompleteValidSet(t *testing.T) {
	engine, tasks, _ := newImportSidecarsFixture(t, map[string]string{
		"research.md":   "# Research\n",
		"decisions.md":  "# Decisions\n",
		"plan.md":       "# Execution Plan\n",
		"brief.md":      "# Final Brief\n",
		"contract.json": validPlanContract("t1"),
	})
	info, _ := tasks.GetTask("t1")
	def, err := engine.store.Get("test-import-sidecars")
	if err != nil {
		t.Fatal(err)
	}

	if ok := engine.adoptSidecarsFromFailedRun("t1", "plan", info, &def); !ok {
		t.Fatal("adoptSidecarsFromFailedRun() = false, want true for a complete valid artifact set")
	}

	got, _ := tasks.GetTask("t1")
	if got.Plan != "# Execution Plan\n" || got.PlanContract == "" {
		t.Errorf("sidecars not adopted: plan=%q contract=%q", got.Plan, got.PlanContract)
	}
}

func TestAdoptSidecarsFromFailedRun_RefusesOnMissingArtifact(t *testing.T) {
	engine, tasks, _ := newImportSidecarsFixture(t, map[string]string{
		"research.md":  "# Research\n",
		"decisions.md": "# Decisions\n",
		"plan.md":      "# Execution Plan\n",
		// brief.md intentionally absent
		"contract.json": validPlanContract("t1"),
	})
	info, _ := tasks.GetTask("t1")
	def, err := engine.store.Get("test-import-sidecars")
	if err != nil {
		t.Fatal(err)
	}

	if ok := engine.adoptSidecarsFromFailedRun("t1", "plan", info, &def); ok {
		t.Fatal("adoptSidecarsFromFailedRun() = true, want false when a declared sidecar is missing")
	}
	got, _ := tasks.GetTask("t1")
	if got.Plan != "" || got.PlanContract != "" {
		t.Errorf("partial adoption occurred: plan=%q contract=%q", got.Plan, got.PlanContract)
	}
}

func TestAdoptSidecarsFromFailedRun_RefusesOnInvalidContract(t *testing.T) {
	engine, tasks, _ := newImportSidecarsFixture(t, map[string]string{
		"research.md":   "# Research\n",
		"decisions.md":  "# Decisions\n",
		"plan.md":       "# Execution Plan\n",
		"brief.md":      "# Final Brief\n",
		"contract.json": `{"task_id": "t1"}`, // missing required fields
	})
	info, _ := tasks.GetTask("t1")
	def, err := engine.store.Get("test-import-sidecars")
	if err != nil {
		t.Fatal(err)
	}

	if ok := engine.adoptSidecarsFromFailedRun("t1", "plan", info, &def); ok {
		t.Fatal("adoptSidecarsFromFailedRun() = true, want false for an invalid plan contract")
	}
	got, _ := tasks.GetTask("t1")
	if got.Plan != "" {
		t.Errorf("adoption occurred despite invalid contract: plan=%q", got.Plan)
	}
}

func TestHandleAgentComplete_FailedRunAdoptsSidecarsInsteadOfConsumingRetry(t *testing.T) {
	engine, tasks, _ := newImportSidecarsFixture(t, map[string]string{
		"research.md":   "# Research\n",
		"decisions.md":  "# Decisions\n",
		"plan.md":       "# Execution Plan\n",
		"brief.md":      "# Final Brief\n",
		"contract.json": validPlanContract("t1"),
	})
	info, _ := tasks.GetTask("t1")
	info.Workflow.AgentRoutes = map[string]string{"a1": "plan"}
	if err := tasks.SetWorkflow("t1", info.Workflow); err != nil {
		t.Fatal(err)
	}

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID: "a1",
		Success: false,
		Result:  "[Read]",
	})

	got, _ := tasks.GetTask("t1")
	if got.Plan == "" || got.PlanContract == "" {
		t.Fatalf("sidecars not adopted after failed run: plan=%q contract=%q", got.Plan, got.PlanContract)
	}
	if got.Workflow == nil || got.Workflow.CountStep("plan") != 1 {
		t.Errorf("plan step retry count = %d, want 1 (adoption must not look like a burned retry)", got.Workflow.CountStep("plan"))
	}
	if got.Status == "human-required" {
		t.Errorf("task escalated to human-required despite adoptable sidecars, reason=%q", got.StatusReason)
	}
}

func TestHandleAgentComplete_FailedRunWithIncompleteArtifactsStillFails(t *testing.T) {
	engine, tasks, _ := newImportSidecarsFixture(t, map[string]string{
		"research.md":  "# Research\n",
		"decisions.md": "# Decisions\n",
		// plan.md, brief.md, contract.json intentionally absent
	})
	info, _ := tasks.GetTask("t1")
	info.Workflow.AgentRoutes = map[string]string{"a1": "plan"}
	if err := tasks.SetWorkflow("t1", info.Workflow); err != nil {
		t.Fatal(err)
	}

	engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID: "a1",
		Success: false,
		Result:  "aborted",
	})

	got, _ := tasks.GetTask("t1")
	if got.Plan != "" || got.PlanContract != "" {
		t.Errorf("sidecars adopted despite incomplete artifact set: plan=%q contract=%q", got.Plan, got.PlanContract)
	}
	if got.Workflow == nil || got.Workflow.CountStep("plan") != 1 {
		t.Errorf("plan step retry count = %d, want 1 for a genuine failed attempt", got.Workflow.CountStep("plan"))
	}
}
