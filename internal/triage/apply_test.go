package triage

import (
	"strings"
	"testing"

	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
)

func newTestManager(t *testing.T) *task.Manager {
	t.Helper()
	dir := t.TempDir()
	s, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return task.NewManager(s, nil)
}

func TestApplyRewritesTitleAndPreservesOriginal(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("i often write random stuff as task name", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	v := Verdict{
		Title:         "feat(triage): rewrite freeform titles into structured form",
		OriginalTitle: created.Title,
		Tags:          []string{"backend", "small", "feature"},
		Size:          "small",
		Type:          "feature",
		Mode:          "headless",
	}
	updated, err := Apply(mgr, created, v, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if updated.Title != v.Title {
		t.Errorf("title not updated: got %q", updated.Title)
	}
	if !strings.Contains(updated.Body, "**Original title:**") {
		t.Errorf("body missing original title marker: %q", updated.Body)
	}
	if !strings.Contains(updated.Body, "i often write random stuff") {
		t.Errorf("body missing original title text: %q", updated.Body)
	}
	if updated.Status != task.StatusTodo {
		t.Errorf("status: got %s, want todo", updated.Status)
	}
	if updated.StatusReason != "triage" {
		t.Errorf("status_reason: got %q", updated.StatusReason)
	}
	if len(updated.Tags) != 3 {
		t.Errorf("tags: got %v", updated.Tags)
	}
}

func TestApplyMediumFeatureGoesToPlanning(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("add auth middleware", "some body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	v := Verdict{
		Title: "feat(auth): add JWT middleware",
		Size:  "medium",
		Type:  "feature",
		Mode:  "headless",
		Tags:  []string{"backend", "medium", "feature"},
	}
	updated, err := Apply(mgr, created, v, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if updated.Status != task.StatusPlanning {
		t.Errorf("status: got %s, want planning", updated.Status)
	}
}

func TestApplyWorkProjectForcesInteractive(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("refactor ingestion", "https://github.com/myco/work-repo/issues/1", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	projects := []project.Project{
		{ID: "myco/work-repo", Owner: "myco", Repo: "work-repo", Type: project.ProjectTypeWork},
	}
	v := Verdict{
		Title: "refactor(ingestion): split pipeline stages",
		Size:  "small",
		Type:  "refactor",
		Mode:  "headless",
		Tags:  []string{"backend", "small", "refactor"},
	}
	updated, err := Apply(mgr, created, v, projects)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if updated.AgentMode != task.AgentModeInteractive {
		t.Errorf("mode: got %q, want interactive", updated.AgentMode)
	}
	if updated.ProjectID != "myco/work-repo" {
		t.Errorf("project_id: got %q", updated.ProjectID)
	}
}

func TestApplyEmptyBodyFillsDescription(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("https://example.com/thing", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	v := Verdict{
		Title:       "docs(thing): update API reference",
		Description: "Update the example.com thing's API docs to match the new schema.",
		Size:        "small",
		Type:        "docs",
		Mode:        "headless",
		Tags:        []string{"docs", "small"},
	}
	updated, err := Apply(mgr, created, v, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(updated.Body, "example.com thing") {
		t.Errorf("body missing description: %q", updated.Body)
	}
}
