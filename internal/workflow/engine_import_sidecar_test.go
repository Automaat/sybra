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
	return NewEngine(store, tasks, agents, discardLogger()), tasks
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
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

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
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

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
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

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
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

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
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

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
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

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
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

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
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

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
