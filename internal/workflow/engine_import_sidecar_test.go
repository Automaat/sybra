package workflow

import (
	"os"
	"path/filepath"
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
}
