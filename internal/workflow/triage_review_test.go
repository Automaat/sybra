package workflow

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/config"
)

func TestTriageVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		files      []string
		insertions int
		deletions  int
		want       string
	}{
		{name: "rename_only_skips", files: []string{"a.go"}, want: triageVerdictSkip},
		{name: "docs_reviews", files: []string{"README.md"}, insertions: 1, want: triageVerdictReview},
		{name: "source_reviews", files: []string{"internal/workflow/a.go"}, insertions: 1, deletions: 1, want: triageVerdictReview},
		{name: "large_diff_reviews", files: []string{"a.go"}, insertions: 5000, want: triageVerdictReview},
		{name: "no_files_reviews", want: triageVerdictReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := triageVerdict(tc.files, tc.insertions, tc.deletions); got != tc.want {
				t.Errorf("triageVerdict = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseShortStat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		wantI int
		wantD int
	}{
		{" 5 files changed, 23 insertions(+), 12 deletions(-)\n", 23, 12},
		{" 1 file changed, 1 insertion(+)\n", 1, 0},
		{" 1 file changed, 4 deletions(-)\n", 0, 4},
		{" 2 files changed, 0 insertions(+), 0 deletions(-)\n", 0, 0},
		{"", 0, 0},
		{"garbage", 0, 0},
	}
	for _, tc := range cases {
		i, d := parseShortStat(tc.in)
		if i != tc.wantI || d != tc.wantD {
			t.Errorf("parseShortStat(%q) = (%d, %d), want (%d, %d)", tc.in, i, d, tc.wantI, tc.wantD)
		}
	}
}

func TestCountPatchLines(t *testing.T) {
	t.Parallel()
	patch := `diff --git a/foo b/foo
--- a/foo
+++ b/foo
@@ -1,2 +1,3 @@
 unchanged
-removed
+added
+another
`
	ins, del := countPatchLines(patch)
	if ins != 2 || del != 1 {
		t.Errorf("countPatchLines = (%d, %d), want (2, 1)", ins, del)
	}
}

func TestSplitNonEmptyLines(t *testing.T) {
	t.Parallel()
	got := splitNonEmptyLines("a\n\nb\n  \nc\n")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func newTriageStep() *Step { return &Step{ID: "triage_review", Type: StepTriageReview} }

func TestExecTriageReview_NoWorktreeNoPRReviews(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	out, err := engine.execTriageReview("t1", newTriageStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != triageVerdictReview {
		t.Errorf("Output = %q, want review (fail-safe)", out.Output)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
}

func TestExecTriageReview_BrokenWorktreeReviews(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	// Path exists but is not a git repo.
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	out, err := engine.execTriageReview("t1", newTriageStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != triageVerdictReview {
		t.Errorf("Output = %q, want review (git error fail-safe)", out.Output)
	}
}

func TestExecTriageReview_TinyDocChangeReviews(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	wt := makeGitRepo(t, false /* extra commit added below */)
	// Add one tiny doc change.
	if err := os.WriteFile(filepath.Join(wt, "NOTES.md"), []byte("note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", "NOTES.md")
	gitRun(t, wt, "commit", "-m", "docs: note")

	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})

	out, err := engine.execTriageReview("t1", newTriageStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != triageVerdictReview {
		t.Errorf("Output = %q, want review", out.Output)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// --- Builtin pick_review_method routing tests ---

func TestBuiltinPRReview_UsesPreparedWorktree(t *testing.T) {
	t.Parallel()

	pr := defByID(t, "pr-review")
	for _, stepID := range []string{"review_simple"} {
		step := pr.StepByID(stepID)
		if step == nil {
			t.Fatalf("%s step not found in pr-review", stepID)
		}
		if !step.Config.NeedsWorktree {
			t.Fatalf("%s needs_worktree = false, want true: inbound reviews must run from the fetched PR checkout", stepID)
		}
	}
}

func TestBuiltinPRReview_AuthorizesVisibleReviewAction(t *testing.T) {
	t.Parallel()

	prompt := renderBuiltinPrompt(t, "pr-review", "review_simple")
	for _, want := range []string{
		"Do not ask the operator for confirmation",
		"NEVER submit an APPROVE review event",
		"if a review for that head already contains the Sybra harness",
		"findings become inline comments",
		"submit with `-f event=REQUEST_CHANGES`",
		"submit with `-f event=COMMENT`",
		"omit `event` so GitHub leaves the review PENDING",
		"_Generated by Sybra harness_",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, banned := range []string{
		"submit a plain comment review",
		"submit a review requesting changes",
		"submit an approve review",
	} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("prompt authorizes %q:\n%s", banned, prompt)
		}
	}
	if strings.Contains(prompt, "event=APPROVE") && !strings.Contains(prompt, "submit `event=APPROVE`") {
		t.Fatalf("prompt authorizes event=APPROVE somewhere unexpected:\n%s", prompt)
	}
}

// TestBuiltinPRReview_EscalatesOnLookupFailure locks the failure-recovery
// contract added for #2647: a review step whose agent could not find/access
// the PR self-escalates to human-required (via sybra-cli), and the step's
// Next transitions must stop the workflow there instead of blindly following
// the unconditional edge into set_in_review — mirroring
// simple-task-review.yaml's require_code_review gate.
func TestBuiltinPRReview_EscalatesOnLookupFailure(t *testing.T) {
	t.Parallel()

	pr := defByID(t, "pr-review")
	for _, stepID := range []string{"review_simple"} {
		step := pr.StepByID(stepID)
		if step == nil {
			t.Fatalf("%s step not found in pr-review", stepID)
		}
		if !strings.Contains(step.Config.Prompt, "human-required") {
			t.Fatalf("%s prompt missing PR-not-found escalation instruction:\n%s", stepID, step.Config.Prompt)
		}
		if len(step.Next) < 2 {
			t.Fatalf("%s next transitions = %+v, want a human-required guard before the unconditional edge", stepID, step.Next)
		}
		guard := step.Next[0]
		if guard.When == nil || guard.When.Field != "task.status" || guard.When.Operator != "equals" ||
			guard.When.Value != "human-required" || guard.GoTo != "" {
			t.Fatalf("%s first transition = %+v, want a human-required guard ending the workflow", stepID, guard)
		}
		fallback := step.Next[len(step.Next)-1]
		if fallback.When != nil || fallback.GoTo != "set_in_review" {
			t.Fatalf("%s fallback transition = %+v, want unconditional goto set_in_review", stepID, fallback)
		}
	}
}

// TestBuiltinSimpleTask_ReviewLoopsUntilClean pins the review→fix→review cycle:
// a clean tamper scan must re-enter review rather than hand off to testing, so
// the fix agent's diff is never the last word. The CLEAN verdict is the only
// exit; there is deliberately no round cap.
func TestBuiltinSimpleTask_ReviewLoopsUntilClean(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-review" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-review not found")
	}

	step := func(id string) *Step {
		for i := range simple.Steps {
			if simple.Steps[i].ID == id {
				return &simple.Steps[i]
			}
		}
		t.Fatalf("step %q not found", id)
		return nil
	}

	// A clean tamper scan re-enters the review picker; a tamper finding still
	// parks the task instead of looping.
	got, err := ResolveTransition(step("detect_tampering").Next, map[string]string{
		"task.status": "testing",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "clear_review_sidecar" {
		t.Errorf("detect_tampering clean goto = %q, want %q — the fix agent's diff must be re-reviewed", got, "clear_review_sidecar")
	}

	// clear_review_sidecar must be reachable ONLY from the back-edge. Its
	// dir renders from `_dir`, which execRunAgent sets only after the first
	// run_agent of this execution starts, and execShell rejects an empty dir
	// outright — so on first entry the step would fail the workflow before any
	// reviewer ran.
	got, err = ResolveTransition(step("triage_review").Next, map[string]string{})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got == "clear_review_sidecar" {
		t.Errorf("triage_review goto = %q — clear_review_sidecar needs _dir, which is unset before the first agent runs", got)
	}
	if got != "pick_review_method" {
		t.Errorf("triage_review goto = %q, want pick_review_method", got)
	}

	// The back-edge must clear the sidecar before re-reviewing, otherwise a
	// failed round re-reads the previous round's verdict and loops forever.
	got, err = ResolveTransition(step("clear_review_sidecar").Next, map[string]string{})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "pick_review_method" {
		t.Errorf("clear_review_sidecar goto = %q, want pick_review_method", got)
	}

	got, err = ResolveTransition(step("detect_tampering").Next, map[string]string{
		"task.status": "human-required",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "" {
		t.Errorf("detect_tampering tampered goto = %q, want \"\" (park, do not loop)", got)
	}

	// CLEAN is the only exit from the loop.
	got, err = ResolveTransition(step("route_review_verdict").Next, map[string]string{
		"vars.review_verdict": "CLEAN",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "done_review" {
		t.Errorf("CLEAN verdict goto = %q, want done_review", got)
	}

	got, err = ResolveTransition(step("route_review_verdict").Next, map[string]string{
		"vars.review_verdict": "NEEDS_FIXES",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "fix_review" {
		t.Errorf("NEEDS_FIXES verdict goto = %q, want fix_review", got)
	}
}

// TestBuiltinSimpleTask_ReviewLoopRespectsConfigToggle pins the kill-switch:
// agent.review_until_clean=false must route the first fix straight to testing
// instead of re-reviewing. Guards the field-name contract too — a
// config.review_until_clean typo here would silently never match and the loop
// would run uncapped regardless of config (the dead-condition bug class
// KnownTriggerFields exists to prevent).
func TestBuiltinSimpleTask_ReviewLoopRespectsConfigToggle(t *testing.T) {
	t.Parallel()

	if !KnownTriggerFields["config.review_until_clean"] {
		t.Fatal("config.review_until_clean missing from KnownTriggerFields — the condition would silently never match")
	}

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-review" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-review not found")
	}
	var tamper *Step
	for i := range simple.Steps {
		if simple.Steps[i].ID == "detect_tampering" {
			tamper = &simple.Steps[i]
			break
		}
	}
	if tamper == nil {
		t.Fatal("detect_tampering step not found")
	}

	cases := []struct {
		name  string
		field string
		want  string
	}{
		{name: "loop_when_enabled", field: "true", want: "clear_review_sidecar"},
		{name: "single_pass_when_disabled", field: "false", want: "done_review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(tamper.Next, map[string]string{
				"task.status":               "testing",
				"config.review_until_clean": tc.field,
			})
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("review_until_clean=%s goto = %q, want %q", tc.field, got, tc.want)
			}
		})
	}

	// Tampering still parks regardless of the toggle.
	got, err := ResolveTransition(tamper.Next, map[string]string{
		"task.status":               "human-required",
		"config.review_until_clean": "false",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "" {
		t.Errorf("tampered goto = %q, want \"\" (park wins over the toggle)", got)
	}
}

func TestBuiltinSimpleTask_ReviewLoopBlocksBudgetExceededBeforeTesting(t *testing.T) {
	t.Parallel()

	if !KnownTriggerFields["task.review_budget_exceeded"] {
		t.Fatal("task.review_budget_exceeded missing from KnownTriggerFields")
	}
	if !KnownTriggerFields["task.review_lifetime_exceeded"] {
		t.Fatal("task.review_lifetime_exceeded missing from KnownTriggerFields")
	}

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-review" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-review not found")
	}
	var tamper *Step
	for i := range simple.Steps {
		if simple.Steps[i].ID == "detect_tampering" {
			tamper = &simple.Steps[i]
			break
		}
	}
	if tamper == nil {
		t.Fatal("detect_tampering step not found")
	}

	got, err := ResolveTransition(tamper.Next, map[string]string{
		"task.status":                 "testing",
		"config.review_until_clean":   "true",
		"task.review_budget_exceeded": "true",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "review_budget_exhausted" {
		t.Fatalf("budget-exceeded goto = %q, want review_budget_exhausted", got)
	}

	limit := simple.StepByID("review_budget_exhausted")
	if limit == nil {
		t.Fatal("review_budget_exhausted step not found")
	}
	if limit.Type != StepSetStatus || limit.Config.Status != "blocked" {
		t.Fatalf("review_budget_exhausted = type %q status %q, want set_status blocked", limit.Type, limit.Config.Status)
	}
	if !strings.Contains(limit.Config.StatusReason, "not eligible for testing or PR creation") {
		t.Fatalf("review_budget_exhausted status_reason = %q, want no-testing-or-PR guidance", limit.Config.StatusReason)
	}

	got, err = ResolveTransition(tamper.Next, map[string]string{
		"task.status":                   "testing",
		"config.review_until_clean":     "true",
		"task.review_budget_exceeded":   "true",
		"task.review_lifetime_exceeded": "true",
	})
	if err != nil {
		t.Fatalf("ResolveTransition lifetime: %v", err)
	}
	if got != "review_lifetime_exhausted" {
		t.Fatalf("lifetime-exceeded goto = %q, want review_lifetime_exhausted", got)
	}

	lifetime := simple.StepByID("review_lifetime_exhausted")
	if lifetime == nil {
		t.Fatal("review_lifetime_exhausted step not found")
	}
	if lifetime.Type != StepSetStatus || lifetime.Config.Status != "human-required" {
		t.Fatalf("review_lifetime_exhausted = type %q status %q, want set_status human-required", lifetime.Type, lifetime.Config.Status)
	}
	if strings.Contains(lifetime.Config.StatusReason, "review_rounds_per_hour") {
		t.Fatalf("review_lifetime_exhausted status_reason = %q, must not prescribe the hourly setting", lifetime.Config.StatusReason)
	}

	var testingTask, prTask *Definition
	for i := range defs {
		switch defs[i].ID {
		case "testing-task":
			testingTask = &defs[i]
		case "simple-task-pr":
			prTask = &defs[i]
		}
	}
	if testingTask == nil {
		t.Fatal("testing-task not found")
	}
	if prTask == nil {
		t.Fatal("simple-task-pr not found")
	}
	blockedFields := map[string]string{
		"task.status": "blocked",
		"task.role":   "",
	}
	if EvalConditions(testingTask.Trigger.Conditions, blockedFields) {
		t.Fatal("blocked review-cap task matched testing-task; it could still reach ready-pr through test PASS")
	}
	if EvalConditions(prTask.Trigger.Conditions, blockedFields) {
		t.Fatal("blocked review-cap task matched simple-task-pr; unresolved review could still open a PR")
	}
}

// TestEngineReviewUntilCleanReachesTransition covers the wiring the YAML-level
// toggle test cannot: SetReviewUntilClean -> reviewLoopDisabled -> the
// config.review_until_clean field resolveNext actually supplies. Without this,
// dropping the `!` from the inverted setter (silently reversing the knob) or
// deleting the field assignment entirely (making the knob dead) both pass the
// whole suite — the config test builds its own field map and never touches
// engine code.
func TestEngineReviewUntilCleanReachesTransition(t *testing.T) {
	t.Parallel()

	// Two steps so resolveNext has a real target to pick, routed purely on the
	// engine-supplied field.
	def := &Definition{
		ID: "test-review-toggle",
		Steps: []Step{
			{
				ID:   "gate",
				Type: StepCondition,
				Next: []Transition{
					{When: &Condition{Field: "config.review_until_clean", Operator: "equals", Value: "false"}, GoTo: "single_pass"},
					{GoTo: "loop"},
				},
			},
			{ID: "single_pass", Type: StepSetStatus},
			{ID: "loop", Type: StepSetStatus},
		},
	}

	cases := []struct {
		name             string
		reviewUntilClean bool
		want             string
	}{
		{name: "default_engine_loops", reviewUntilClean: true, want: "loop"},
		{name: "disabled_takes_single_pass", reviewUntilClean: false, want: "single_pass"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mt := newMemTasks()
			mt.tasks["t1"] = &TaskInfo{ID: "t1", Status: "testing"}
			e := &Engine{logger: slog.New(slog.DiscardHandler), tasks: mt}
			e.SetReviewUntilClean(tc.reviewUntilClean)

			wfExec := &Execution{WorkflowID: def.ID, Variables: map[string]string{}}
			next, _, err := e.resolveNext("t1", def, &def.Steps[0], wfExec, TaskInfo{ID: "t1", Status: "testing"})
			if err != nil {
				t.Fatalf("resolveNext: %v", err)
			}
			if next == nil {
				t.Fatalf("resolveNext returned no step, want %q", tc.want)
			}
			if next.ID != tc.want {
				t.Errorf("SetReviewUntilClean(%v) routed to %q, want %q — the toggle is inverted or unwired",
					tc.reviewUntilClean, next.ID, tc.want)
			}
		})
	}
}

// A zero-value Engine must keep the loop on, matching config.ReviewUntilClean's
// documented default, since the field is stored inverted precisely so an engine
// built without the setter does not silently ship single-pass review.
func TestEngineReviewUntilCleanDefaultsToLooping(t *testing.T) {
	t.Parallel()

	mt := newMemTasks()
	mt.tasks["t1"] = &TaskInfo{ID: "t1"}
	e := &Engine{logger: slog.New(slog.DiscardHandler), tasks: mt}
	def := &Definition{ID: "d", Steps: []Step{
		{ID: "gate", Type: StepCondition, Next: []Transition{
			{When: &Condition{Field: "config.review_until_clean", Operator: "equals", Value: "false"}, GoTo: "single_pass"},
			{GoTo: "loop"},
		}},
		{ID: "single_pass", Type: StepSetStatus},
		{ID: "loop", Type: StepSetStatus},
	}}

	next, _, err := e.resolveNext("t1", def, &def.Steps[0], &Execution{Variables: map[string]string{}}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("resolveNext: %v", err)
	}
	if next == nil || next.ID != "loop" {
		t.Fatalf("zero-value engine routed to %v, want loop", next)
	}
}

// TestEngineReviewBudgetExceededTransition locks in that simple-task-review's
// detect_tampering condition is bounded by the same shared reviewbudget.Budget
// the inbound PR-review dispatcher uses (#2499) — not a separate
// per-workflow-execution round counter — so AgentRuns are stamped relative to
// time.Now(), not a fixed Workflow.StartedAt.
func TestEngineReviewBudgetExceededTransition(t *testing.T) {
	t.Parallel()

	def := &Definition{
		ID: "test-review-budget",
		Steps: []Step{
			{
				ID:   "gate",
				Type: StepCondition,
				Next: []Transition{
					{When: &Condition{Field: "task.review_budget_exceeded", Operator: "equals", Value: "true"}, GoTo: "park"},
					{GoTo: "loop"},
				},
			},
			{ID: "park", Type: StepSetStatus},
			{ID: "loop", Type: StepSetStatus},
		},
	}

	mt := newMemTasks()
	mt.tasks["t1"] = &TaskInfo{ID: "t1", Status: "testing"}
	e := &Engine{logger: slog.New(slog.DiscardHandler), tasks: mt}
	e.SetReviewUntilClean(true)
	e.SetReviewRoundsPerHour(2)
	fakeClock := clock.NewFake(time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC))
	e.SetClock(fakeClock)

	now := fakeClock.Now()
	task := TaskInfo{
		ID:     "t1",
		Status: "testing",
		AgentRuns: []AgentRunInfo{
			{Role: "review", StartedAt: now.Add(-50 * time.Minute)},
			{Role: "review", StartedAt: now.Add(-10 * time.Minute)},
		},
	}
	next, _, err := e.resolveNext("t1", def, &def.Steps[0], &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("resolveNext: %v", err)
	}
	if next == nil || next.ID != "park" {
		t.Fatalf("resolveNext routed to %v, want park", next)
	}

	fakeClock.Advance(time.Hour)
	next, _, err = e.resolveNext("t1", def, &def.Steps[0], &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("resolveNext after advancing clock: %v", err)
	}
	if next == nil || next.ID != "loop" {
		t.Fatalf("resolveNext after advancing clock routed to %v, want loop", next)
	}
}

func TestEngineReviewBudgetExceededTransition_LifetimeCap(t *testing.T) {
	t.Parallel()

	def := &Definition{
		ID: "test-review-budget-lifetime",
		Steps: []Step{
			{
				ID:   "gate",
				Type: StepCondition,
				Next: []Transition{
					{When: &Condition{Field: "task.review_lifetime_exceeded", Operator: "equals", Value: "true"}, GoTo: "park"},
					{GoTo: "loop"},
				},
			},
			{ID: "park", Type: StepSetStatus},
			{ID: "loop", Type: StepSetStatus},
		},
	}

	mt := newMemTasks()
	mt.tasks["t1"] = &TaskInfo{ID: "t1", Status: "testing"}
	e := &Engine{logger: slog.New(slog.DiscardHandler), tasks: mt}
	e.SetReviewUntilClean(true)
	e.SetReviewRoundsPerHour(-1)

	now := time.Now()
	task := TaskInfo{
		ID:     "t1",
		Status: "testing",
		AgentRuns: []AgentRunInfo{
			{Role: "review", StartedAt: now.Add(-48 * time.Hour), Outcome: "success"},
			{Role: "review", StartedAt: now.Add(-36 * time.Hour), Outcome: "success"},
			{Role: "review", StartedAt: now.Add(-24 * time.Hour), Outcome: "success"},
			{Role: "review", StartedAt: now.Add(-12 * time.Hour), Outcome: "success"},
			{Role: "review", StartedAt: now.Add(-6 * time.Hour), Outcome: "success"},
			{Role: "review", StartedAt: now.Add(-2 * time.Hour), Outcome: "success"},
		},
	}
	next, _, err := e.resolveNext("t1", def, &def.Steps[0], &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("resolveNext: %v", err)
	}
	if next == nil || next.ID != "park" {
		t.Fatalf("resolveNext routed to %v, want park", next)
	}
}

// TestPrimaryLoopCapsBoundAgentRuns locks the combined ceiling for the four
// primary loops changed by #2759 to the limits the engine and builtin
// workflows actually resolve. It includes run_agent max_retries and the three
// independently keyed route_test_result recovery budgets. Orthogonal watchdog,
// provider, and worktree recovery ladders have their own focused bound tests.
// Review arithmetic is deliberately conservative: the lifetime cap counts
// reviewer retry attempts themselves, so the real review total is lower.
func TestPrimaryLoopCapsBoundAgentRuns(t *testing.T) {
	t.Parallel()

	configured := &config.Config{}
	configured.Testing.MaxAttempts = 25 // legacy deployments must be clamped.
	testingRuns := configured.TestingMaxAttempts()
	if testingRuns != config.DefaultTestingMaxAttempts {
		t.Fatalf("resolved testing runs = %d, want safety ceiling %d", testingRuns, config.DefaultTestingMaxAttempts)
	}

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	stepAttempts := func(workflowID, stepID string) int {
		t.Helper()
		for i := range defs {
			if defs[i].ID != workflowID {
				continue
			}
			step := defs[i].StepByID(stepID)
			if step == nil {
				t.Fatalf("workflow %s step %s not found", workflowID, stepID)
			}
			return 1 + step.Config.MaxRetries
		}
		t.Fatalf("workflow %s not found", workflowID)
		return 0
	}

	implementationDispatches := testingRuns * (1 + verifyChecksAutoFixCeiling + verifyChecksAutoFixCeiling)
	implementationRuns := implementationDispatches * stepAttempts("simple-task-implement", "implement")
	testingRetryOutcomes := []string{
		testOutcomeInfraFailure,
		testOutcomeMissingEvidence,
		testOutcomeProtocolViolation,
	}
	seenRetryKeys := make(map[string]bool, len(testingRetryOutcomes))
	for _, outcome := range testingRetryOutcomes {
		key := testingAutoRetryKey(outcome)
		if seenRetryKeys[key] {
			t.Fatalf("testing retry outcomes share counter %q", key)
		}
		seenRetryKeys[key] = true
	}
	testerDispatches := testingRuns * (1 + len(testingRetryOutcomes)*testingAutoRetryCap)
	testerAttempts := testerDispatches * stepAttempts("testing-task", "run_test")
	reviewRuns := config.DefaultReviewRoundsPerTask
	reviewAttempts := reviewRuns * stepAttempts("simple-task-review", "code_review_simple")
	fixReviewRuns := reviewRuns * stepAttempts("simple-task-review", "fix_review")

	const want = 211
	got := implementationRuns + testerAttempts + reviewAttempts + fixReviewRuns
	if got != want {
		t.Fatalf("worst-case agent-run ceiling = %d (implementation=%d testing=%d review=%d fix-review=%d), want %d",
			got, implementationRuns, testerAttempts, reviewAttempts, fixReviewRuns, want)
	}

	now := time.Now()
	runs := make([]AgentRunInfo, 0, reviewRuns)
	for i := range reviewRuns - 1 {
		runs = append(runs, AgentRunInfo{Role: "review", StartedAt: now.Add(-time.Duration(i+2) * time.Hour), Outcome: "success"})
	}
	e := &Engine{}
	e.SetReviewRoundsPerHour(-1)
	if _, lifetime := e.reviewBudgetExhaustion(TaskInfo{AgentRuns: runs}); lifetime {
		t.Fatalf("lifetime review ceiling fired after %d runs, want room for one more", len(runs))
	}
	runs = append(runs, AgentRunInfo{Role: "review", StartedAt: now.Add(-24 * time.Hour), Outcome: "success"})
	if _, lifetime := e.reviewBudgetExhaustion(TaskInfo{AgentRuns: runs}); !lifetime {
		t.Fatalf("lifetime review ceiling did not fire after %d runs", len(runs))
	}
}

func TestReviewBudgetExhaustion_StalledRunsDoNotSpendTheCeiling(t *testing.T) {
	t.Parallel()
	now := time.Now()
	e := &Engine{}
	e.SetReviewRoundsPerHour(-1)

	runs := make([]AgentRunInfo, 0, 6)
	for i := range 6 {
		runs = append(runs, AgentRunInfo{
			Role: "review", StartedAt: now.Add(-time.Duration(i+2) * time.Hour), TurnCount: 4,
		})
	}
	if _, lifetime := e.reviewBudgetExhaustion(TaskInfo{AgentRuns: runs}); lifetime {
		t.Fatal("six runs that recorded no outcome — rate-limited, watchdog-stopped or superseded — spent the whole ceiling without reviewing anything")
	}
}
