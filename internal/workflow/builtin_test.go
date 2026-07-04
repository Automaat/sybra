package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestBuiltinSimpleTask_MaybeCritiqueReplanSkip locks the behavior that
// critique_plan runs only on the first plan pass. On replan (after a human
// reject), `vars.step.review_plan.output` exists, so maybe_critique must
// route directly to review_plan and spare the critic a second run on a
// plan the human already eyeballed once. Substring-checking tag "nocritic"
// is still honored as an opt-out on the first pass.
func TestBuiltinSimpleTask_MaybeCritiqueReplanSkip(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
	}
	step := simple.StepByID("maybe_critique")
	if step == nil {
		t.Fatal("maybe_critique step not found in simple-task-plan")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name: "first_pass_runs_critique",
			fields: map[string]string{
				"task.tags": "backend,feature",
			},
			want: "critique_plan",
		},
		{
			name: "nocritic_tag_skips_critique",
			fields: map[string]string{
				"task.tags": "backend,nocritic",
			},
			want: "review_plan",
		},
		{
			name: "replan_skips_critique_even_without_nocritic",
			fields: map[string]string{
				"task.tags":                    "backend,feature",
				"vars.step.review_plan.output": "reject",
				"vars.human_action":            "reject",
			},
			want: "review_plan",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuiltinSimpleTask_MissingCritiqueSkipsToHumanReview(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
	}
	step := simple.StepByID("require_plan_critique")
	if step == nil {
		t.Fatal("require_plan_critique step not found in simple-task-plan")
	}
	if !step.Config.AllowMissing {
		t.Fatal("require_plan_critique must soft-fail when the critic produces no sidecar")
	}
	got, err := ResolveTransition(step.Next, map[string]string{
		"vars.step.require_plan_critique.output": "plan critique missing — upstream agent step completed without writing its sidecar — skipped (non-fatal)",
	})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "review_plan" {
		t.Fatalf("missing critique next = %q, want review_plan", got)
	}
}

func TestBuiltinSimpleTask_AddressCritiqueRevalidatesPlanArtifacts(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
	}
	step := simple.StepByID("address_critique")
	if step == nil {
		t.Fatal("address_critique step not found in simple-task-plan")
	}
	got, err := ResolveTransition(step.Next, map[string]string{"task.status": "plan-review"})
	if err != nil {
		t.Fatalf("ResolveTransition: %v", err)
	}
	if got != "require_plan_after_address" {
		t.Fatalf("address_critique next = %q, want require_plan_after_address", got)
	}

	chain := []struct {
		step string
		want string
	}{
		{"require_plan_after_address", "require_plan_decisions_after_address"},
		{"require_plan_decisions_after_address", "require_plan_brief_after_address"},
		{"require_plan_brief_after_address", "require_plan_research_after_address"},
		{"require_plan_research_after_address", "validate_plan_refs_after_address"},
		{"validate_plan_refs_after_address", "validate_plan_contract_after_address"},
		{"validate_plan_contract_after_address", "review_plan"},
	}
	for _, c := range chain {
		step := simple.StepByID(c.step)
		if step == nil {
			t.Fatalf("%s step not found in simple-task-plan", c.step)
		}
		got, err := ResolveTransition(step.Next, map[string]string{"task.status": "plan-review"})
		if err != nil {
			t.Fatalf("ResolveTransition(%s): %v", c.step, err)
		}
		if got != c.want {
			t.Fatalf("%s next = %q, want %q", c.step, got, c.want)
		}
	}
}

// The planning pipeline runs autonomously with no human attached. Every
// run_agent step must therefore be headless: an interactive-mode agent is
// skipped by the watchdog (internal/watchdog only supervises ag.Mode ==
// "headless"), so a hang leaves the workflow waiting forever with no stall
// recovery — the failure mode that wedged task 3c1c4f12 in `planning`.
func TestBuiltinSimpleTaskPlan_AgentStepsAreHeadless(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
	}

	var agentSteps int
	for i := range simple.Steps {
		step := &simple.Steps[i]
		if step.Type != StepRunAgent {
			continue
		}
		agentSteps++
		if step.Config.Mode != "headless" {
			t.Errorf("run_agent step %q mode = %q, want headless (autonomous planning agents must stay under watchdog supervision)", step.ID, step.Config.Mode)
		}
	}
	if agentSteps == 0 {
		t.Fatal("expected simple-task-plan to contain run_agent steps")
	}
}

func TestBuiltinSimpleTaskImplement_UsesCompactTaskView(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var impl *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-implement" {
			impl = &defs[i]
			break
		}
	}
	if impl == nil {
		t.Fatal("simple-task-implement builtin definition not found")
	}
	step := impl.StepByID("implement")
	if step == nil {
		t.Fatal("implement step not found in simple-task-implement")
	}
	if !strings.Contains(step.Config.Prompt, "sybra-cli get --compact {{.Task.ID}}") {
		t.Fatalf("implement prompt must use compact task view, got:\n%s", step.Config.Prompt)
	}
}

// TestBuiltinSimpleTaskImplement_VerifyCommitsRouting pins the verify_commits
// transition table: human-required and done end the run, everything else flows
// into the detect_tampering gate before review. (The sibling-still-running case
// is handled in Go by parking the workflow in ExecWaiting, not by a transition
// — see execVerifyCommits.)
func TestBuiltinSimpleTaskImplement_VerifyCommitsRouting(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var impl *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-implement" {
			impl = &defs[i]
			break
		}
	}
	if impl == nil {
		t.Fatal("simple-task-implement builtin definition not found")
	}
	step := impl.StepByID("verify_commits")
	if step == nil {
		t.Fatal("verify_commits step not found in simple-task-implement")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "human-required ends the run",
			fields: map[string]string{"task.status": "human-required"},
			want:   "",
		},
		{
			name:   "done ends the run",
			fields: map[string]string{"task.status": "done"},
			want:   "",
		},
		{
			name:   "clean in-progress flows into tamper gate",
			fields: map[string]string{"task.status": "in-progress"},
			want:   "detect_tampering",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuiltinSimpleTask_TriageNoplanRouting locks the noplan escape hatch in
// the triage step's transition table: a noplan tag routes straight to
// implement (winning over a planning status), terminal statuses still win over
// noplan, and the absence of noplan preserves the normal plan/implement split.
func TestBuiltinSimpleTask_TriageNoplanRouting(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-plan" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-plan builtin definition not found")
	}
	step := simple.StepByID("triage")
	if step == nil {
		t.Fatal("triage step not found in simple-task-plan")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "planning_without_noplan_goes_to_plan",
			fields: map[string]string{"task.status": "planning", "task.tags": "backend,feature"},
			want:   "plan",
		},
		{
			name:   "noplan_skips_planning_even_when_status_planning",
			fields: map[string]string{"task.status": "planning", "task.tags": "backend,noplan"},
			want:   "set_in_progress_and_end",
		},
		{
			name:   "todo_without_noplan_hands_off_to_implement",
			fields: map[string]string{"task.status": "todo", "task.tags": "backend"},
			want:   "set_in_progress_and_end",
		},
		{
			name:   "terminal_status_wins_over_noplan",
			fields: map[string]string{"task.status": "done", "task.tags": "backend,noplan"},
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuiltinSimpleTaskReview_MaybeReviewTrivialRouting locks the trivial
// escape hatch in simple-task-review's maybe_review transition table: a
// trivial tag routes straight to done_review (skipping the code-review
// agents), same as an already-reviewed task, while noreview and the normal
// path are unaffected.
func TestBuiltinSimpleTaskReview_MaybeReviewTrivialRouting(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var review *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-review" {
			review = &defs[i]
			break
		}
	}
	if review == nil {
		t.Fatal("simple-task-review builtin definition not found")
	}
	step := review.StepByID("maybe_review")
	if step == nil {
		t.Fatal("maybe_review step not found in simple-task-review")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "no_escape_hatch_goes_to_triage_review",
			fields: map[string]string{"task.reviewed": "false", "task.tags": "backend,feature"},
			want:   "triage_review",
		},
		{
			name:   "trivial_skips_review",
			fields: map[string]string{"task.reviewed": "false", "task.tags": "backend,trivial"},
			want:   "done_review",
		},
		{
			name:   "noreview_skips_review",
			fields: map[string]string{"task.reviewed": "false", "task.tags": "backend,noreview"},
			want:   "done_review",
		},
		{
			name:   "already_reviewed_wins_over_normal_path",
			fields: map[string]string{"task.reviewed": "true", "task.tags": "backend,feature"},
			want:   "done_review",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuiltinDefinitions(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("expected non-empty builtin definitions")
	}
	for _, d := range defs {
		if d.ID == "" {
			t.Errorf("builtin definition has empty ID: %+v", d)
		}
		if !d.Builtin {
			t.Errorf("builtin definition %q has Builtin=false", d.ID)
		}
	}
}

func TestBuiltinDefinitions_Valid(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for _, d := range defs {
		t.Run(d.ID, func(t *testing.T) {
			t.Parallel()
			if err := d.Validate(); err != nil {
				t.Errorf("Validate() error for %q: %v", d.ID, err)
			}
		})
	}
}

func TestBuiltinPRFix_RoutesAgentHumanRequiredBeforePRRelink(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var prfix *Definition
	for i := range defs {
		if defs[i].ID == "pr-fix" {
			prfix = &defs[i]
			break
		}
	}
	if prfix == nil {
		t.Fatal("pr-fix builtin definition not found")
	}
	fix := prfix.StepByID("fix")
	if fix == nil {
		t.Fatal("fix step missing from pr-fix")
	}
	if got, err := ResolveTransition(fix.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "route_pr_fix_result" {
		t.Fatalf("fix next = %q, err=%v; want route_pr_fix_result", got, err)
	}
	route := prfix.StepByID("route_pr_fix_result")
	if route == nil {
		t.Fatal("route_pr_fix_result step missing from pr-fix")
	}
	if route.Type != StepRoutePRFixResult {
		t.Fatalf("route_pr_fix_result type = %q, want %q", route.Type, StepRoutePRFixResult)
	}
	if got, err := ResolveTransition(route.Next, map[string]string{"task.status": "human-required"}); err != nil || got != "" {
		t.Fatalf("human-required route next = %q, err=%v; want end", got, err)
	}
	if got, err := ResolveTransition(route.Next, map[string]string{"task.status": "in-progress"}); err != nil || got != "verify_commits" {
		t.Fatalf("default route next = %q, err=%v; want verify_commits", got, err)
	}
}

func TestBuiltinSimpleTaskReview_CreatePRUsesForkRemote(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-pr" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-pr builtin definition not found")
	}
	step := simple.StepByID("create_pr")
	if step == nil {
		t.Fatal("create_pr step not found in simple-task-pr")
	}
	prompt := step.Config.Prompt
	for _, want := range []string{
		`remote.fork.url`,
		`PUSH_REMOTE="fork"`,
		`REMOTE_URL="$(git config --get "remote.$PUSH_REMOTE.url")"`,
		`--head "$HEAD_ARG"`,
		`git push -u "$PUSH_REMOTE" HEAD:"$BRANCH"`,
		`headRefOid`,
		`test "$LOCAL_SHA" = "$PR_SHA"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("create_pr prompt missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"git push -u origin HEAD",
		"git push --force-with-lease -u origin HEAD",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("create_pr prompt still hardcodes %q", forbidden)
		}
	}
}

// TestBuiltinSimpleTaskPR_SyncBranchPrecedesPRHandoff proves the proactive
// sync_branch step runs first — before the create_pr-vs-push_existing_pr
// branch point — so both PR handoff paths (new PR and retry-with-pr_number)
// get a fresh sync before the branch is pushed.
func TestBuiltinSimpleTaskPR_SyncBranchPrecedesPRHandoff(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-pr" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task-pr builtin definition not found")
	}

	first := simple.FirstStep()
	if first == nil || first.Type != StepSyncBranch {
		t.Fatalf("FirstStep = %+v, want sync_branch first", first)
	}

	syncStep := simple.StepByID("sync_branch")
	if syncStep == nil {
		t.Fatal("sync_branch step not found in simple-task-pr")
	}
	if len(syncStep.Next) != 1 || syncStep.Next[0].GoTo != "maybe_create_pr" {
		t.Fatalf("sync_branch.Next = %+v, want unconditional goto maybe_create_pr", syncStep.Next)
	}

	// maybe_create_pr must still be the branch point covering both downstream
	// PR paths — sync_branch does not bypass either one.
	guard := simple.StepByID("maybe_create_pr")
	if guard == nil {
		t.Fatal("maybe_create_pr step not found in simple-task-pr")
	}
	var gotoTargets []string
	for _, n := range guard.Next {
		gotoTargets = append(gotoTargets, n.GoTo)
	}
	if !slices.Contains(gotoTargets, "push_existing_pr") || !slices.Contains(gotoTargets, "create_pr") {
		t.Fatalf("maybe_create_pr.Next targets = %v, want both push_existing_pr and create_pr", gotoTargets)
	}
}

func TestBuiltinDefinitions_NoDuplicateIDs(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	seen := make(map[string]bool)
	for _, d := range defs {
		if seen[d.ID] {
			t.Errorf("duplicate builtin ID: %q", d.ID)
		}
		seen[d.ID] = true
	}
}

func TestSyncBuiltins(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != len(defs) {
		t.Errorf("store has %d workflows, want %d", len(listed), len(defs))
	}
}

func TestSyncBuiltins_NoOverwrite(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v (len=%d)", err, len(defs))
	}

	// Save a modified version of the first builtin.
	modified := defs[0]
	modified.Name = "user-modified"
	modified.Builtin = false // simulate user edit
	if err := store.Save(modified); err != nil {
		t.Fatalf("Save modified: %v", err)
	}

	// SyncBuiltins must not overwrite.
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	got, err := store.Get(modified.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "user-modified" {
		t.Errorf("SyncBuiltins overwrote user modification: got %q, want %q", got.Name, "user-modified")
	}
}

// TestSyncBuiltins_OverwriteStaleBuiltin locks the drift-repair behavior:
// a stored workflow still marked Builtin=true must get replaced when its
// content diverges from the embedded copy. Matches the historical pr-fix
// drift that left `operator: contains` on a scalar enum field and silently
// broke all auto-fix dispatch.
func TestSyncBuiltins_OverwriteStaleBuiltin(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v (len=%d)", err, len(defs))
	}

	// Simulate drift: save with Builtin=true but a stale name.
	stale := defs[0]
	stale.Name = "stale-drifted-name"
	stale.Builtin = true
	if err := store.Save(stale); err != nil {
		t.Fatalf("Save stale: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	got, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != defs[0].Name {
		t.Errorf("SyncBuiltins did not repair stale builtin: got %q, want %q",
			got.Name, defs[0].Name)
	}
}

func TestSyncBuiltins_PrunesObsoleteBuiltin(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	obsolete := newTestDef("obsolete-builtin-sync-test")
	obsolete.Builtin = true
	if err := store.Save(obsolete); err != nil {
		t.Fatalf("Save obsolete: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	if _, err := store.Get(obsolete.ID); err == nil {
		t.Fatalf("obsolete builtin %q still exists", obsolete.ID)
	}
}

func TestSyncBuiltins_PreservesObsoleteUserWorkflow(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	custom := newTestDef("obsolete-custom-workflow-sync-test")
	custom.Builtin = false
	if err := store.Save(custom); err != nil {
		t.Fatalf("Save custom: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	if _, err := store.Get(custom.ID); err != nil {
		t.Fatalf("custom workflow was pruned: %v", err)
	}
}

func TestSyncBuiltins_PruneFailureDoesNotBlockRefresh(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	bad := []byte(`id: ../obsolete-builtin-sync-test
name: Bad Builtin
trigger:
  on: task.created
steps:
  - id: s
    type: set_status
    config:
      status: todo
builtin: true
`)
	if err := os.WriteFile(filepath.Join(store.Dir(), "bad.yaml"), bad, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err = SyncBuiltins(store)
	if err == nil {
		t.Fatal("SyncBuiltins succeeded, want prune error")
	}
	defs, defsErr := BuiltinDefinitions()
	if defsErr != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v (len=%d)", defsErr, len(defs))
	}
	if _, getErr := store.Get(defs[0].ID); getErr != nil {
		t.Fatalf("current builtin was not refreshed after prune failure: %v", getErr)
	}
}

// TestSyncBuiltins_IdempotentOnClean verifies the no-op path: calling
// SyncBuiltins twice in a row on a freshly seeded store must not bounce
// the UpdatedAt timestamp on every startup.
func TestSyncBuiltins_IdempotentOnClean(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("first SyncBuiltins: %v", err)
	}
	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	before, err := store.Get(defs[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("second SyncBuiltins: %v", err)
	}
	after, err := store.Get(defs[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("idempotent sync rewrote file: UpdatedAt before=%v after=%v",
			before.UpdatedAt, after.UpdatedAt)
	}
}

// TestBuiltinTestingTask_RunTestOutputSchema verifies that the testing-task
// builtin's run_test step carries a non-empty OutputSchema that round-trips
// through YAML without alteration. Exact-string assertion so YAML folding
// or escaping errors surface immediately rather than silently passing an
// empty schema to codex.
func TestBuiltinTestingTask_RunTestOutputSchema(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	step := testingDef.StepByID("run_test")
	if step == nil {
		t.Fatal("run_test step not found in testing-task")
	}
	const wantSchema = `{"type":"object","properties":{"verdict":{"type":"string","enum":["PASS","FAIL"]},"outcome":{"type":"string","enum":["pass","product_bug","infra_failure","ambiguous_requirement","missing_evidence"]},"failures_markdown":{"type":"string"},"surface_kind":{"type":"string","enum":["web","cli","server","desktop","k8s","library","docs","none"]},"app_started":{"type":"boolean"},"start_command":{"type":"string"},"readiness_probe":{"type":"object","properties":{"command":{"type":"string"},"expected":{"type":"string"},"actual":{"type":"string"},"observed":{"type":"string"},"output":{"type":"string"},"status":{"type":"string"},"url":{"type":"string"}},"required":["command","expected","actual","observed","output","status","url"],"additionalProperties":false},"manual_probes":{"type":"array","items":{"type":"object","properties":{"command":{"type":"string"},"expected":{"type":"string"},"actual":{"type":"string"},"observed":{"type":"string"},"output":{"type":"string"},"status":{"type":"string"}},"required":["command","expected","actual","observed","output","status"],"additionalProperties":false}},"automated_checks":{"type":"array","items":{"type":"object","properties":{"command":{"type":"string"},"actual":{"type":"string"},"observed":{"type":"string"},"output":{"type":"string"},"status":{"type":"string"}},"required":["command","actual","observed","output","status"],"additionalProperties":false}},"unable_to_run_reason":{"type":"string"}},"required":["verdict","outcome","failures_markdown","surface_kind","app_started","start_command","readiness_probe","manual_probes","automated_checks","unable_to_run_reason"],"additionalProperties":false}`
	if step.Config.OutputSchema != wantSchema {
		t.Errorf("run_test.Config.OutputSchema =\n%q\nwant:\n%q", step.Config.OutputSchema, wantSchema)
	}

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal([]byte(step.Config.OutputSchema), &schema); err != nil {
		t.Fatalf("unmarshal output schema: %v", err)
	}
	required := map[string]bool{}
	for _, field := range schema.Required {
		required[field] = true
	}
	for field := range schema.Properties {
		if !required[field] {
			t.Fatalf("codex strict output schema requires every property; %q missing from required", field)
		}
	}
	requireCodexStrictOutputSchema(t, []byte(step.Config.OutputSchema))
}

func requireCodexStrictOutputSchema(t *testing.T, raw []byte) {
	t.Helper()

	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshal output schema: %v", err)
	}
	requireCodexStrictSchemaNode(t, schema, "$")
}

func requireCodexStrictSchemaNode(t *testing.T, node any, path string) {
	t.Helper()

	switch v := node.(type) {
	case map[string]any:
		_, hasAdditionalProperties := v["additionalProperties"]
		if isCodexObjectSchema(v) && !hasAdditionalProperties {
			t.Fatalf("%s is object-shaped and must set additionalProperties:false", path)
		}
		propsValue, hasProps := v["properties"]
		if _, hasRequired := v["required"]; hasRequired && !hasProps {
			t.Fatalf("%s uses required without properties; Codex strict rejects requirement-only schemas", path)
		}
		if hasProps {
			props, ok := propsValue.(map[string]any)
			if !ok {
				t.Fatalf("%s.properties is %T, want object", path, propsValue)
			}
			additionalProperties, ok := v["additionalProperties"].(bool)
			if !ok || additionalProperties {
				t.Fatalf("%s must set additionalProperties:false", path)
			}
			requiredValue, ok := v["required"].([]any)
			if !ok {
				t.Fatalf("%s must require every property", path)
			}
			required := make(map[string]bool, len(requiredValue))
			for _, field := range requiredValue {
				name, ok := field.(string)
				if !ok {
					t.Fatalf("%s required field is %T, want string", path, field)
				}
				required[name] = true
			}
			for field := range props {
				if !required[field] {
					t.Fatalf("%s property %q missing from required", path, field)
				}
			}
		}
		for key, child := range v {
			requireCodexStrictSchemaNode(t, child, path+"."+key)
		}
	case []any:
		for _, child := range v {
			requireCodexStrictSchemaNode(t, child, path+"[]")
		}
	}
}

func isCodexObjectSchema(schema map[string]any) bool {
	if schemaType, ok := schema["type"].(string); ok && schemaType == "object" {
		return true
	}
	_, hasProperties := schema["properties"]
	_, hasRequired := schema["required"]
	_, hasAdditionalProperties := schema["additionalProperties"]
	return hasProperties || hasRequired || hasAdditionalProperties
}

// TestBuiltinTestingTask_NotestStillRunsTester asserts the real invariant:
// notest only downgrades evidence requirements (app-start exemption), it
// must never bypass the test-runner. skip_testing exists (for `trivial`) but
// notest-tagged tasks must still route to run_test.
func TestBuiltinTestingTask_NotestStillRunsTester(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	maybe := testingDef.StepByID("maybe_test")
	if maybe == nil {
		t.Fatal("maybe_test step not found in testing-task")
	}
	for _, n := range maybe.Next {
		if n.When != nil && n.When.Value == "notest" {
			t.Fatalf("maybe_test must not branch on notest, got branch to %q", n.GoTo)
		}
	}
	if got := maybe.Next[len(maybe.Next)-1].GoTo; got != "run_test" {
		t.Fatalf("maybe_test fallthrough = %q, want run_test", got)
	}
}

func TestBuiltinTestingTask_TrivialSkipsTester(t *testing.T) {
	t.Parallel()

	testingDef := mustBuiltinDefinition(t, "testing-task")
	maybe := testingDef.StepByID("maybe_test")
	if maybe == nil {
		t.Fatal("maybe_test step not found in testing-task")
	}
	var gotTrivialBranch bool
	for _, n := range maybe.Next {
		if n.When != nil && n.When.Field == "task.tags" && n.When.Operator == "contains" && n.When.Value == "trivial" {
			gotTrivialBranch = true
			if n.GoTo != "skip_testing" {
				t.Fatalf("trivial branch goto = %q, want skip_testing", n.GoTo)
			}
		}
	}
	if !gotTrivialBranch {
		t.Fatal("maybe_test has no branch for task.tags contains trivial")
	}

	skip := testingDef.StepByID("skip_testing")
	if skip == nil {
		t.Fatal("skip_testing step not found in testing-task")
	}
	if skip.Type != "set_status" || skip.Config.Status != "ready-pr" {
		t.Fatalf("skip_testing = %+v, want set_status to ready-pr", skip)
	}
}

func mustBuiltinDefinition(t *testing.T, id string) *Definition {
	t.Helper()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for i := range defs {
		if defs[i].ID == id {
			return &defs[i]
		}
	}
	t.Fatalf("%s builtin definition not found", id)
	return nil
}
