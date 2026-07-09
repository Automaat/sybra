package triage

import (
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
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
	if updated.StatusReason != "" {
		t.Errorf("status_reason: got %q, want empty (reason reserved for attention states)", updated.StatusReason)
	}
	if len(updated.Tags) != 3 {
		t.Errorf("tags: got %v", updated.Tags)
	}
}

func TestApplyPreservesEscapeHatchTags(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("trivial typo fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A human/orchestrator set noplan on the task before triage runs.
	created.Tags = []string{"noplan"}

	// Classifier verdict does not include noplan (it's outside the vocabulary).
	v := Verdict{
		Title: "fix(docs): typo",
		Tags:  []string{"docs", "small", "docs"},
		Size:  "small",
		Type:  "docs",
		Mode:  "headless",
	}
	updated, err := Apply(mgr, created, v, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(updated.Tags, "noplan") {
		t.Errorf("noplan escape-hatch tag dropped by triage; got %v", updated.Tags)
	}
}

func TestApplyPreservesHumanSetTrivialTag(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("trivial typo fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A human/orchestrator set trivial on the task before triage runs.
	created.Tags = []string{"trivial"}

	// This verdict omits trivial; Apply must preserve the pre-set escape hatch.
	v := Verdict{
		Title: "fix(docs): typo",
		Tags:  []string{"docs", "small", "docs"},
		Size:  "small",
		Type:  "docs",
		Mode:  "headless",
	}
	updated, err := Apply(mgr, created, v, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(updated.Tags, "trivial") {
		t.Errorf("trivial escape-hatch tag dropped by triage; got %v", updated.Tags)
	}
}

func TestApplyPreservesUmbrellaGatedTag(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("gated child task", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The umbrella expander sets the gate marker before triage runs.
	created.Tags = []string{umbrella.GatedTag}

	// Classifier verdict does not include umbrella-gated (it's outside the vocabulary).
	v := Verdict{
		Title: "feat(github): skip re-polling known-green PRs before merge",
		Tags:  []string{"backend", "medium"},
		Size:  "medium",
		Type:  "feature",
		Mode:  "headless",
	}
	updated, err := Apply(mgr, created, v, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(updated.Tags, umbrella.GatedTag) {
		t.Errorf("umbrella-gated tag dropped by triage; got %v", updated.Tags)
	}
}

func TestApplyKeepsClassifierEmittedNoplanOnWorkProject(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("bump dep on work repo", "https://github.com/example-org/example-repo/pull/9", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	projects := []project.Project{
		{ID: "example-org/example-repo", Owner: "example-org", Repo: "example-repo", Type: project.ProjectTypeWork},
	}
	// Headline case: the classifier itself emits noplan for a trivially
	// mechanical work-typed task. Run the full emit→validate→apply path (the
	// task has no pre-existing escape-hatch tag, so this is emission, not the
	// human-set preservation path). ValidateVerdict's floor must keep noplan
	// (small + chore qualifies); Apply must persist it so the workflow skips
	// planning even though RouteStatus still parks the task at status=planning.
	v := Verdict{
		Title: "chore(deps): bump dependency",
		Tags:  []string{"infra", "small", "chore", "noplan"},
		Size:  "small",
		Type:  "chore",
		Mode:  "headless",
	}
	if err := ValidateVerdict(&v); err != nil {
		t.Fatalf("ValidateVerdict: %v", err)
	}
	updated, err := Apply(mgr, created, v, projects)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(updated.Tags, "noplan") {
		t.Errorf("classifier-emitted noplan tag not persisted; got %v", updated.Tags)
	}
	if updated.Status != task.StatusPlanning {
		t.Errorf("status: got %s, want planning (workflow skips via noplan tag, not status)", updated.Status)
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

func TestApplyWorkProjectForcesInteractiveAndPlanning(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("refactor ingestion", "https://github.com/example-org/example-repo/issues/1", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	projects := []project.Project{
		{ID: "example-org/example-repo", Owner: "example-org", Repo: "example-repo", Type: project.ProjectTypeWork},
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
	if updated.Status != task.StatusPlanning {
		t.Errorf("status: got %s, want planning", updated.Status)
	}
	if updated.ProjectID != "example-org/example-repo" {
		t.Errorf("project_id: got %q", updated.ProjectID)
	}
}

func TestApplyUsesExistingProjectWhenTaskHasNoRepoURL(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("debug workflow completion race", "no repository URL here", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created, err = mgr.Update(created.ID, task.Update{ProjectID: task.Ptr("example-org/example-repo")})
	if err != nil {
		t.Fatalf("Update project: %v", err)
	}
	projects := []project.Project{
		{ID: "example-org/example-repo", Owner: "example-org", Repo: "example-repo", Type: project.ProjectTypeWork},
	}
	v := Verdict{
		Title: "fix(workflow): handle completion race",
		Size:  "small",
		Type:  "bug",
		Mode:  "headless",
		Tags:  []string{"backend", "small", "bug"},
	}
	updated, err := Apply(mgr, created, v, projects)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if updated.ProjectID != "example-org/example-repo" {
		t.Errorf("project_id: got %q, want example-org/example-repo", updated.ProjectID)
	}
	if updated.AgentMode != task.AgentModeInteractive {
		t.Errorf("mode: got %q, want interactive", updated.AgentMode)
	}
	if updated.Status != task.StatusPlanning {
		t.Errorf("status: got %s, want planning", updated.Status)
	}
}

func TestApplyExistingProjectWinsOverClassifierGuess(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("migrate vault e2e", "https://github.com/kumahq/kuma/blob/main/test/framework.go", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created, err = mgr.Update(created.ID, task.Update{ProjectID: task.Ptr("kong/kong-mesh")})
	if err != nil {
		t.Fatalf("Update project: %v", err)
	}
	projects := []project.Project{
		{ID: "kong/kong-mesh", Owner: "kong", Repo: "kong-mesh", Type: project.ProjectTypeWork},
		{ID: "kumahq/kuma", Owner: "kumahq", Repo: "kuma", Type: project.ProjectTypeWork},
	}
	v := Verdict{
		Title:     "test(mesh): migrate vault e2e",
		Size:      "medium",
		Type:      "refactor",
		Mode:      "headless",
		Tags:      []string{"backend", "test", "medium", "refactor"},
		ProjectID: "kumahq/kuma",
	}
	updated, err := Apply(mgr, created, v, projects)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if updated.ProjectID != "kong/kong-mesh" {
		t.Errorf("project_id: got %q, want kong/kong-mesh (existing routing must win over classifier guess)", updated.ProjectID)
	}
}

func TestApplyPRFixRunRoleNeverPlanning(t *testing.T) {
	mgr := newTestManager(t)
	// Create a work-project task that would normally go to planning.
	created, err := mgr.Create("Fix CI: bump lodash", "https://github.com/example-org/example-repo/pull/42", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Pre-set RunRole as FixRenovateCI would via CreateFull.
	created.RunRole = "pr-fix"
	created.PRNumber = 42

	projects := []project.Project{
		{ID: "example-org/example-repo", Owner: "example-org", Repo: "example-repo", Type: project.ProjectTypeWork},
	}
	v := Verdict{
		Title: "chore(deps): fix CI on bump lodash",
		Size:  "large",
		Type:  "feature",
		Mode:  "headless",
		Tags:  []string{"backend", "large", "feature"},
	}
	updated, err := Apply(mgr, created, v, projects)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// pr-fix floor must override both work-project → planning and large-feature → planning.
	if updated.Status != task.StatusTodo {
		t.Errorf("status: got %s, want todo (pr-fix floor)", updated.Status)
	}
}

func TestApplyPRNumberNeverPlanning(t *testing.T) {
	mgr := newTestManager(t)
	created, err := mgr.Create("Fix some CI", "https://github.com/example-org/example-repo/pull/7", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created.PRNumber = 7

	projects := []project.Project{
		{ID: "example-org/example-repo", Owner: "example-org", Repo: "example-repo", Type: project.ProjectTypeWork},
	}
	v := Verdict{
		Title: "feat(ci): fix broken pipeline",
		Size:  "medium",
		Type:  "feature",
		Mode:  "headless",
		Tags:  []string{"ci", "medium", "feature"},
	}
	updated, err := Apply(mgr, created, v, projects)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if updated.Status != task.StatusTodo {
		t.Errorf("status: got %s, want todo (pr_number floor)", updated.Status)
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
