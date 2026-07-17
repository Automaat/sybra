package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newClearStep() *Step {
	return &Step{
		ID:   "clear_plan_artifacts",
		Type: StepClearPlanArtifacts,
		Config: StepConfig{
			ClearSidecars:      []string{"plan", "plan_research", "plan_decisions", "plan_brief", "plan_contract", "plan_critique"},
			ClearWorktreeGlobs: []string{".sybra-plan-*", ".sybra-critique-*"},
		},
	}
}

// writeCycle1Artifacts lays down what a completed planning cycle leaves behind:
// the sidecars on the task, and the agent's files in the worktree.
func writeCycle1Artifacts(t *testing.T, dir string) {
	t.Helper()
	files := []string{
		".sybra-plan-t1.md",
		".sybra-plan-research-t1.md",
		".sybra-plan-decisions-t1.md",
		".sybra-plan-brief-t1.md",
		".sybra-plan-contract-t1.json",
		".sybra-critique-t1.md",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("cycle 1 content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func clearTestEnv(t *testing.T, dir string) (*Engine, *memTasks, TaskInfo) {
	t.Helper()
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	info := TaskInfo{
		ID:            "t1",
		Plan:          "cycle 1 plan",
		PlanResearch:  "cycle 1 research",
		PlanDecisions: "# Decisions\n\nNo open decisions.",
		PlanBrief:     "cycle 1 brief",
		PlanContract:  `{"cycle":1}`,
		PlanCritique:  "cycle 1 critique",
		Workflow:      &Execution{Variables: map[string]string{WorkflowVarDir: dir}},
	}
	tasks.Put(info)
	return engine, tasks, info
}

// A replan reuses the worktree and .sybra-plan-* is git-excluded rather than
// deleted, so cycle 2 would otherwise re-import cycle 1's bytes as though the
// current planner had just written them (#2191).
func TestClearPlanArtifacts_RemovesSidecarsAndWorktreeFiles(t *testing.T) {
	dir := t.TempDir()
	writeCycle1Artifacts(t, dir)
	engine, tasks, info := clearTestEnv(t, dir)

	out, err := engine.execClearPlanArtifacts("t1", newClearStep(), info)
	if err != nil {
		t.Fatalf("execClearPlanArtifacts: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}

	got, _ := tasks.GetTask("t1")
	for name, v := range map[string]string{
		"plan": got.Plan, "plan_research": got.PlanResearch, "plan_decisions": got.PlanDecisions,
		"plan_brief": got.PlanBrief, "plan_contract": got.PlanContract, "plan_critique": got.PlanCritique,
	} {
		if v != "" {
			t.Errorf("%s sidecar = %q, want cleared: cycle 2 must not inherit it", name, v)
		}
	}

	survivors, _ := filepath.Glob(filepath.Join(dir, ".sybra-*"))
	if len(survivors) != 0 {
		t.Errorf("worktree files survived: %v — the next import reads these straight back", survivors)
	}
}

// The critique file does not share the plan prefix, so clearing its sidecar
// without listing .sybra-critique-* would leave the file to be re-imported —
// the same half-cleared state the step exists to prevent.
func TestClearPlanArtifacts_ClearsTheCritiqueFileToo(t *testing.T) {
	dir := t.TempDir()
	writeCycle1Artifacts(t, dir)
	engine, _, info := clearTestEnv(t, dir)

	if _, err := engine.execClearPlanArtifacts("t1", newClearStep(), info); err != nil {
		t.Fatalf("execClearPlanArtifacts: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".sybra-critique-t1.md")); !os.IsNotExist(err) {
		t.Error(".sybra-critique-t1.md survived; its sidecar was cleared, so the stale file would be re-imported")
	}
}

// Files the planning cycle does not own must survive: NOTES.md is the agent's
// memory across runs and .sybra-context.md is its identity beacon.
func TestClearPlanArtifacts_LeavesUnrelatedWorktreeFilesAlone(t *testing.T) {
	dir := t.TempDir()
	writeCycle1Artifacts(t, dir)
	keep := map[string]string{
		"NOTES.md":          "agent working memory",
		".sybra-context.md": "identity beacon",
		"main.go":           "package main",
	}
	for name, content := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	engine, _, info := clearTestEnv(t, dir)

	if _, err := engine.execClearPlanArtifacts("t1", newClearStep(), info); err != nil {
		t.Fatalf("execClearPlanArtifacts: %v", err)
	}

	for name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was deleted; the step must only clear the planning cycle's own artifacts", name)
		}
	}
}

// Fails closed: replanning onto artifacts it could not clear is how a stale
// affirmation reaches a human gate that then auto-approves it.
func TestClearPlanArtifacts_UnknownWorktreeEscalatesRatherThanReplanning(t *testing.T) {
	tasks := newMemTasks()
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	info := TaskInfo{
		ID:       "t1",
		Plan:     "cycle 1 plan",
		Workflow: &Execution{Variables: map[string]string{}}, // no _dir
	}
	tasks.Put(info)

	out, err := engine.execClearPlanArtifacts("t1", newClearStep(), info)
	if err == nil {
		t.Fatal("expected the step to park: it cannot know the previous cycle's files are gone")
	}
	if out.Status != "failed" {
		t.Errorf("status = %q, want failed", out.Status)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("status = %q, want human-required: a replan onto stale artifacts must never proceed silently", got.Status)
	}
	if !strings.Contains(got.StatusReason, "re-import") {
		t.Errorf("reason = %q, want it to name the consequence", got.StatusReason)
	}
}

// A worktree that is genuinely gone holds no stale files to serve, so there is
// nothing to clear and nothing to fail closed over.
func TestClearPlanArtifacts_MissingWorktreeDirIsNotAFailure(t *testing.T) {
	engine, tasks, info := clearTestEnv(t, filepath.Join(t.TempDir(), "worktree-was-cleaned-up"))

	if _, err := engine.execClearPlanArtifacts("t1", newClearStep(), info); err != nil {
		t.Fatalf("execClearPlanArtifacts: %v", err)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status == "human-required" {
		t.Error("escalated over an absent worktree, which cannot be serving stale content")
	}
	if got.PlanDecisions != "" {
		t.Error("sidecars must still be cleared when the worktree is gone — they are what the fail-open reads")
	}
}

func TestClearPlanArtifacts_NothingConfiguredIsAnError(t *testing.T) {
	engine, _, info := clearTestEnv(t, t.TempDir())
	step := &Step{ID: "clear", Type: StepClearPlanArtifacts}

	if _, err := engine.execClearPlanArtifacts("t1", step, info); err == nil {
		t.Fatal("a clear step that clears nothing is a silent no-op; it must not be accepted")
	}
}

func planWorkflowDef(t *testing.T) *Definition {
	t.Helper()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			return &defs[i]
		}
	}
	t.Fatal("simple-task-plan builtin not found")
	return nil
}

// The replan path had no test at all, which is how start_replan came to clear
// nothing while looking like it prepared a cycle (#2191).
func TestBuiltinSimpleTask_ReplanClearsBeforeReplanning(t *testing.T) {
	t.Parallel()
	def := planWorkflowDef(t)

	replan := def.StepByID("start_replan")
	if replan == nil {
		t.Fatal("start_replan step missing")
	}
	if len(replan.Next) != 1 || replan.Next[0].GoTo != "clear_plan_artifacts" {
		t.Fatalf("start_replan goes to %+v, want clear_plan_artifacts: re-entering plan directly replans onto the last cycle's artifacts", replan.Next)
	}

	clearStep := def.StepByID("clear_plan_artifacts")
	if clearStep == nil {
		t.Fatal("clear_plan_artifacts step missing")
	}
	if clearStep.Type != StepClearPlanArtifacts {
		t.Errorf("type = %q, want %q", clearStep.Type, StepClearPlanArtifacts)
	}
	if len(clearStep.Next) != 1 || clearStep.Next[0].GoTo != "plan" {
		t.Fatalf("clear_plan_artifacts goes to %+v, want plan", clearStep.Next)
	}
}

// Every artifact the plan step imports must be cleared before a replan, both
// its sidecar and its file. A kind covered by neither is re-served from the
// previous cycle, and require_sidecar cannot catch it: those guards assert
// non-empty, and stale content is non-empty.
//
// This is the invariant rather than a fixed list, so a future import_sidecars
// entry cannot quietly reintroduce the bug.
func TestBuiltinSimpleTask_ReplanClearsEveryImportedPlanArtifact(t *testing.T) {
	t.Parallel()
	def := planWorkflowDef(t)

	clearStep := def.StepByID("clear_plan_artifacts")
	if clearStep == nil {
		t.Fatal("clear_plan_artifacts step missing")
	}
	cleared := map[string]bool{}
	for _, k := range clearStep.Config.ClearSidecars {
		cleared[k] = true
	}

	for _, step := range def.Steps {
		if step.Type != StepRunAgent {
			continue
		}
		imports := step.Config.ImportSidecars
		if step.Config.ImportSidecar != nil {
			imports = append(imports, *step.Config.ImportSidecar)
		}
		for _, imp := range imports {
			// Only planning-cycle artifacts are invalidated by a replan; a
			// review/test artifact belongs to a different phase.
			if !strings.HasPrefix(imp.Kind, "plan") {
				continue
			}
			if !cleared[imp.Kind] {
				t.Errorf("step %q imports sidecar %q but clear_plan_artifacts does not clearStep it: a replan would re-serve the previous cycle's value", step.ID, imp.Kind)
			}
			if !globsCover(clearStep.Config.ClearWorktreeGlobs, imp.From) {
				t.Errorf("step %q imports %q from %q, which no clear_worktree_globs entry matches: the file survives and is read straight back", step.ID, imp.Kind, imp.From)
			}
		}
	}
}

// globsCover reports whether one of globs matches the basename pattern of a
// sidecar's `from` template (templates render an ID, so compare the prefix).
func globsCover(globs []string, from string) bool {
	base := from
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	for _, g := range globs {
		prefix := strings.TrimSuffix(g, "*")
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}
