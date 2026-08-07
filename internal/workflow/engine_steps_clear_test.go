package workflow

import (
	"os"
	"path/filepath"
	"regexp"
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
			panic("unreachable")
		}
	}
}

func clearTestEnv(t *testing.T, dir string) (*Engine, *memTasks, TaskInfo) {
	t.Helper()
	tasks := newMemTasks()
	engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
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
		panic("unreachable")
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
		panic("unreachable")
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
			panic("unreachable")
		}
	}
	engine, _, info := clearTestEnv(t, dir)

	if _, err := engine.execClearPlanArtifacts("t1", newClearStep(), info); err != nil {
		t.Fatalf("execClearPlanArtifacts: %v", err)
		panic("unreachable")
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
	engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	info := TaskInfo{
		ID:       "t1",
		Plan:     "cycle 1 plan",
		Workflow: &Execution{Variables: map[string]string{}}, // no _dir
	}
	tasks.Put(info)

	out, err := engine.execClearPlanArtifacts("t1", newClearStep(), info)
	if err != nil {
		t.Fatalf("escalation must not error: the human-required edge halts the workflow, and a bare error strands the execution at a step nothing resumes: %v", err)
		panic("unreachable")
	}
	if strings.Contains(out.Output, "cleared") {
		t.Errorf("output = %q, want a blocked reason", out.Output)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("status = %q, want human-required: a replan onto stale artifacts must never proceed silently", got.Status)
	}
	if !strings.Contains(got.StatusReason, "re-import") {
		t.Errorf("reason = %q, want it to name the consequence", got.StatusReason)
	}
}

// A worktree that is genuinely ABSENT holds no stale files to serve, so there
// is nothing to clear and nothing to fail closed over. Only ENOENT qualifies —
// see the sibling test below for why that distinction is the whole ballgame.
func TestClearPlanArtifacts_MissingWorktreeDirIsNotAFailure(t *testing.T) {
	engine, tasks, info := clearTestEnv(t, filepath.Join(t.TempDir(), "worktree-was-cleaned-up"))

	if _, err := engine.execClearPlanArtifacts("t1", newClearStep(), info); err != nil {
		t.Fatalf("execClearPlanArtifacts: %v", err)
		panic("unreachable")
	}
	got, _ := tasks.GetTask("t1")
	if got.Status == "human-required" {
		t.Error("escalated over an absent worktree, which cannot be serving stale content")
	}
	if got.PlanDecisions != "" {
		t.Error("sidecars must still be cleared when the worktree is gone — they are what the fail-open reads")
	}
}

// An unreadable worktree is not an absent one. Treating every stat error as
// "gone" reports success over a worktree still holding cycle 1's decisions, and
// cycle 2 then walks straight back into the fail-open this step exists to close
// — the original bug, reached through the fix's own hole.
func TestClearPlanArtifacts_UnreadableWorktreeEscalates(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root traverses any permission bit")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "worktree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	writeCycle1Artifacts(t, dir)
	// Block the traverse so Stat fails with EACCES rather than ENOENT: the
	// files are still there, we simply cannot see them.
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	engine, tasks, info := clearTestEnv(t, dir)
	out, err := engine.execClearPlanArtifacts("t1", newClearStep(), info)
	if err != nil {
		t.Fatalf("execClearPlanArtifacts: %v", err)
		panic("unreachable")
	}
	if strings.Contains(out.Output, "cleared") {
		t.Errorf("output = %q, want a blocked reason: it reported success over files it never even saw", out.Output)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("status = %q, want human-required: cycle 1's decisions survive in that worktree", got.Status)
	}
}

// If the escalation flip itself cannot be persisted, the human-required edge
// never fires, and the unconditional goto:plan would carry on over the very
// half-cleared cycle this step exists to catch. A failure at that point must
// halt the workflow with a hard error instead of quietly reporting completed.
func TestClearPlanArtifacts_StatusUpdateFailureIsHardError(t *testing.T) {
	tasks := newMemTasks()
	engine := NewTestEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	// The task is never Put, so UpdateTaskStatus fails with "not found" —
	// standing in for a store write failure at exactly the point it matters.
	info := TaskInfo{
		ID:       "missing-task",
		Workflow: &Execution{Variables: map[string]string{}}, // no _dir, so clearWorktreeGlob fails too
	}
	step := &Step{
		ID:     "clear_plan_artifacts",
		Type:   StepClearPlanArtifacts,
		Config: StepConfig{ClearWorktreeGlobs: []string{".sybra-plan-*"}},
	}

	if _, err := engine.execClearPlanArtifacts("missing-task", step, info); err == nil {
		t.Fatal("a status-update failure must halt the workflow with a hard error, not report completed")
		panic("unreachable")
	}
}

func TestClearPlanArtifacts_NothingConfiguredIsAnError(t *testing.T) {
	engine, _, info := clearTestEnv(t, t.TempDir())
	step := &Step{ID: "clear", Type: StepClearPlanArtifacts}

	if _, err := engine.execClearPlanArtifacts("t1", step, info); err == nil {
		t.Fatal("a clear step that clears nothing is a silent no-op; it must not be accepted")
		panic("unreachable")
	}
}

func planWorkflowDef(t *testing.T) *Definition {
	t.Helper()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
		panic("unreachable")
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
		panic("unreachable")
	}
	if len(replan.Next) != 1 || replan.Next[0].GoTo != "clear_plan_artifacts" {
		t.Fatalf("start_replan goes to %+v, want clear_plan_artifacts: re-entering plan directly replans onto the last cycle's artifacts", replan.Next)
	}

	clearStep := def.StepByID("clear_plan_artifacts")
	if clearStep == nil {
		t.Fatal("clear_plan_artifacts step missing")
		panic("unreachable")
	}
	if clearStep.Type != StepClearPlanArtifacts {
		t.Errorf("type = %q, want %q", clearStep.Type, StepClearPlanArtifacts)
	}
	// Two edges: halt on the escalation it raises when it cannot clear, then
	// plan. Without the first, a blocked clear replans onto stale artifacts —
	// the exact thing it just refused to do.
	if len(clearStep.Next) != 2 {
		t.Fatalf("clear_plan_artifacts has %d edges, want 2 (human-required halt, then plan): %+v", len(clearStep.Next), clearStep.Next)
	}
	halt := clearStep.Next[0]
	if halt.When == nil || halt.When.Value != "human-required" || halt.GoTo != "" {
		t.Errorf("first edge = %+v, want a human-required halt", halt)
	}
	if clearStep.Next[1].GoTo != "plan" {
		t.Errorf("second edge goes to %q, want plan", clearStep.Next[1].GoTo)
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
		panic("unreachable")
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
				t.Errorf("step %q imports sidecar %q but clear_plan_artifacts does not clear it: a replan would re-serve the previous cycle's value", step.ID, imp.Kind)
			}
			if !globsCover(clearStep.Config.ClearWorktreeGlobs, imp.From) {
				t.Errorf("step %q imports %q from %q, which no clear_worktree_globs entry matches: the file survives and is read straight back", step.ID, imp.Kind, imp.From)
			}
		}
	}
}

// globsCover reports whether one of globs matches the file a sidecar imports.
//
// Uses filepath.Match rather than a prefix check so the invariant reflects what
// clearWorktreeGlob actually does at runtime — a prefix test would pass globs
// that Glob would never match, and quietly bless a file the replan leaves
// behind.
func globsCover(globs []string, from string) bool {
	base := from
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	// `from` is a template; render the task-ID placeholder to a representative
	// filename so Match sees the real shape.
	base = taskIDTemplate.ReplaceAllString(base, "task123")
	for _, g := range globs {
		if ok, err := filepath.Match(g, base); err == nil && ok {
			return true
		}
	}
	return false
}

var taskIDTemplate = regexp.MustCompile(`\{\{[^}]*\}\}`)

// filepath.Glob reports a bad pattern but never a directory it could not read,
// so an unreadable worktree returns zero matches and no error. Statting the dir
// cannot see that: the dir exists, it just cannot be listed — and the step would
// report "cleared" while every cycle-1 file sits there, serving the next import.
func TestClearPlanArtifacts_UnreadableWorktreeDirEscalates(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any directory")
	}
	dir := t.TempDir()
	writeCycle1Artifacts(t, dir)
	// The dir itself is unreadable, but its parent is traversable — so Stat
	// succeeds where ReadDir fails. That gap is the whole point.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	engine, tasks, info := clearTestEnv(t, dir)
	out, err := engine.execClearPlanArtifacts("t1", newClearStep(), info)
	if err != nil {
		t.Fatalf("must escalate, not error out: %v", err)
		panic("unreachable")
	}
	if strings.Contains(out.Output, "cleared") {
		t.Errorf("output = %q, want a blocked reason: Glob saw nothing because it could not read the dir, not because it was empty", out.Output)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("status = %q, want human-required: cycle 1's files are still in that worktree", got.Status)
	}
}

// A configured glob that escapes the worktree ('..', an absolute path, or a
// nested path separator) would turn this cleanup step into a delete primitive
// pointed anywhere this process can reach. Every shape must escalate rather
// than run, and — the actual guarantee, not just the error path — must never
// touch a file planted outside the worktree it was scoped to.
func TestClearPlanArtifacts_EscapingGlobEscalatesAndLeavesOutsideFilesAlone(t *testing.T) {
	tests := []struct {
		name string
		glob string
	}{
		{"parent traversal", "../*"},
		{"absolute path", "/etc/*"},
		{"nested path", "sub/dir/*"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "worktree")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
				panic("unreachable")
			}
			writeCycle1Artifacts(t, dir)
			outside := filepath.Join(parent, "outside-marker")
			if err := os.WriteFile(outside, []byte("must survive"), 0o644); err != nil {
				t.Fatal(err)
				panic("unreachable")
			}

			engine, tasks, info := clearTestEnv(t, dir)
			step := &Step{
				ID:   "clear_plan_artifacts",
				Type: StepClearPlanArtifacts,
				Config: StepConfig{
					ClearSidecars:      []string{"plan"},
					ClearWorktreeGlobs: []string{tt.glob},
				},
			}

			out, err := engine.execClearPlanArtifacts("t1", step, info)
			if err != nil {
				t.Fatalf("must escalate, not error out: %v", err)
				panic("unreachable")
			}
			if strings.Contains(out.Output, "cleared") {
				t.Errorf("output = %q, want a blocked reason", out.Output)
			}
			got, _ := tasks.GetTask("t1")
			if got.Status != "human-required" {
				t.Errorf("status = %q, want human-required: an escaping glob must never run", got.Status)
			}
			if _, err := os.Stat(outside); err != nil {
				t.Errorf("outside marker file did not survive: %v", err)
			}
		})
	}
}

// A blank entry is a config mistake, not a request to clear nothing: it must
// not let the "nothing configured to clear" guard pass while actually
// clearing zero files.
func TestClearPlanArtifacts_BlankGlobEscalates(t *testing.T) {
	dir := t.TempDir()
	writeCycle1Artifacts(t, dir)
	engine, tasks, info := clearTestEnv(t, dir)
	step := &Step{
		ID:   "clear_plan_artifacts",
		Type: StepClearPlanArtifacts,
		Config: StepConfig{
			ClearSidecars:      []string{"plan"},
			ClearWorktreeGlobs: []string{"  "},
		},
	}

	out, err := engine.execClearPlanArtifacts("t1", step, info)
	if err != nil {
		t.Fatalf("must escalate, not error out: %v", err)
		panic("unreachable")
	}
	if strings.Contains(out.Output, "cleared") {
		t.Errorf("output = %q, want a blocked reason", out.Output)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("status = %q, want human-required: a blank glob must not pass as a no-op", got.Status)
	}
}
