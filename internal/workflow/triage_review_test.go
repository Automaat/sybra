package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func manyGoFiles(n int) []string {
	files := make([]string, 0, n)
	for i := range n {
		files = append(files, fmt.Sprintf("pkg/svc/file%d.go", i))
	}

	return files
}

func TestTriageVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		files      []string
		insertions int
		deletions  int
		want       string
	}{
		{
			name:       "tiny_doc_change_is_simple",
			files:      []string{"README.md"},
			insertions: 3,
			deletions:  1,
			want:       "simple",
		},
		{
			name:       "small_frontend_tweak_is_simple",
			files:      []string{"frontend/src/App.svelte", "frontend/src/style.css"},
			insertions: 12,
			deletions:  4,
			want:       "simple",
		},
		{
			name:       "single_line_workflow_change_still_staff",
			files:      []string{"internal/workflow/engine.go"},
			insertions: 1,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "single_line_agent_change_still_staff",
			files:      []string{"internal/agent/manager.go"},
			insertions: 5,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "test_file_alone_is_simple",
			files:      []string{"internal/task/parser_test.go"},
			insertions: 8,
			deletions:  2,
			want:       "simple",
		},
		{
			name:       "test_file_under_risky_path_is_staff",
			files:      []string{"internal/workflow/engine_test.go"},
			insertions: 8,
			deletions:  2,
			want:       "staff",
		},
		{
			name:       "ci_workflow_always_staff",
			files:      []string{".github/workflows/ci.yml"},
			insertions: 4,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "dockerfile_always_staff",
			files:      []string{"Dockerfile"},
			insertions: 3,
			deletions:  1,
			want:       "staff",
		},
		{
			name:       "main_go_is_simple",
			files:      []string{"main.go"},
			insertions: 2,
			deletions:  1,
			want:       "simple",
		},
		{
			name:       "routine_feature_sized_diff_is_simple",
			files:      []string{"frontend/src/App.svelte"},
			insertions: 100,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "many_lines_is_staff",
			files:      []string{"frontend/src/App.svelte"},
			insertions: triageReviewLineLimit + 1,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "handful_of_files_is_simple",
			files:      []string{"a.go", "b.go", "c.go", "d.go"},
			insertions: 4,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "many_files_is_staff",
			files:      manyGoFiles(triageReviewFileLimit + 1),
			insertions: 4,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "dep_bump_is_simple",
			files:      []string{"go.mod", "go.sum"},
			insertions: 30,
			deletions:  10,
			want:       "simple",
		},
		{
			name:       "generated_bindings_is_simple",
			files:      []string{"frontend/bindings/github.com/Automaat/sybra/internal/sybra/taskservice.ts"},
			insertions: 20,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "generated_pb_go_is_simple",
			files:      []string{"internal/proto/task.pb.go"},
			insertions: 20,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "generated_suffix_gen_go_is_simple",
			files:      []string{"pkg/generated/client_gen.go"},
			insertions: 10,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "generated_suffix_dot_gen_go_is_simple",
			files:      []string{"pkg/generated/client.gen.go"},
			insertions: 10,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "large_docs_only_is_simple",
			files:      []string{"docs/CONFIG.md"},
			insertions: triageReviewLineLimit + 100,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "many_docs_only_is_simple",
			files:      []string{"a.md", "b.md", "c.md", "d.md", "e.md"},
			insertions: 5,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "docs_under_risky_internal_path_is_simple",
			files:      []string{"internal/workflow/README.md"},
			insertions: 5,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "skill_md_carve_out_is_staff",
			files:      []string{".claude/skills/sybra-tasks/SKILL.md"},
			insertions: 3,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "claude_skills_dotmd_carve_out_precedence_over_docs_ext",
			files:      []string{".claude/skills/foo.md"},
			insertions: 3,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "claude_md_carve_out_is_staff",
			files:      []string{"CLAUDE.md"},
			insertions: 3,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "mixed_trivial_and_source_is_staff",
			files:      []string{"README.md", "internal/agent/x.go"},
			insertions: 3,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "generated_file_under_risky_path_is_staff",
			files:      []string{"internal/workflow/ast_gen.go"},
			insertions: 3,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "uppercase_extension_large_diff_bypasses_fast_path_stays_staff",
			files:      []string{"BIG.MD"},
			insertions: triageReviewLineLimit + 50,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "no_extension_risky_dockerfile_is_staff",
			files:      []string{"Dockerfile"},
			insertions: 1,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "unknown_extension_small_diff_is_simple",
			files:      []string{"schema.sql"},
			insertions: 5,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "binary_or_no_extension_small_diff_is_simple",
			files:      []string{"bin/runner"},
			insertions: 1,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "zero_lines_changed_is_skip",
			files:      []string{"README.md"},
			insertions: 0,
			deletions:  0,
			want:       "skip",
		},
		{
			name:       "zero_lines_multiple_files_is_skip",
			files:      []string{"a.go", "b.go"},
			insertions: 0,
			deletions:  0,
			want:       "skip",
		},
		{
			name:       "no_files_is_staff",
			files:      nil,
			insertions: 0,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "right_at_line_limit_still_simple",
			files:      []string{"docs.md"},
			insertions: triageReviewLineLimit,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "right_at_file_limit_still_simple",
			files:      []string{"a.md", "b.md", "c.md"},
			insertions: 3,
			deletions:  0,
			want:       "simple",
		},
		{
			name:       "large_dep_bump_now_stays_staff",
			files:      []string{"go.mod", "go.sum"},
			insertions: triageReviewLineLimit + 1,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "large_generated_diff_now_stays_staff",
			files:      []string{"frontend/bindings/github.com/Automaat/sybra/internal/sybra/taskservice.ts"},
			insertions: triageReviewLineLimit + 1,
			deletions:  0,
			want:       "staff",
		},
		{
			name:       "non_internal_go_outside_risky_paths_simple",
			files:      []string{"pkg/util/strings.go"},
			insertions: 10,
			deletions:  2,
			want:       "simple",
		},
		{
			name:       "package_json_dep_bump_is_simple",
			files:      []string{"package.json"},
			insertions: 4,
			deletions:  1,
			want:       "simple",
		},
		{
			name:       "package_lock_dep_bump_is_simple",
			files:      []string{"package-lock.json"},
			insertions: 4,
			deletions:  1,
			want:       "simple",
		},
		{
			name:       "yarn_lock_dep_bump_is_simple",
			files:      []string{"yarn.lock"},
			insertions: 4,
			deletions:  1,
			want:       "simple",
		},
		{
			name:       "pnpm_lock_dep_bump_is_simple",
			files:      []string{"pnpm-lock.yaml"},
			insertions: 4,
			deletions:  1,
			want:       "simple",
		},
		{
			name:       "renovate_policy_file_small_diff_is_simple_without_trivial_fast_path",
			files:      []string{"renovate.json"},
			insertions: 4,
			deletions:  1,
			want:       "simple",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := triageVerdict(tc.files, tc.insertions, tc.deletions)
			if got != tc.want {
				t.Errorf("verdict = %q, want %q (reason: %s)", got, tc.want, reason)
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

func TestFilepathExt(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"foo.go":             ".go",
		"x/y/foo.tsx":        ".tsx",
		"Dockerfile":         "",
		"a.b.c":              ".c",
		"":                   "",
		"foo/bar":            "",
		".github/workflows/": "",
	}
	for in, want := range cases {
		if got := filepathExt(in); got != want {
			t.Errorf("filepathExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func newTriageStep() *Step { return &Step{ID: "triage_review", Type: StepTriageReview} }

func TestExecTriageReview_NoWorktreeNoPRReturnsStaff(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	out, err := engine.execTriageReview("t1", newTriageStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "staff" {
		t.Errorf("Output = %q, want staff (fail-safe)", out.Output)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
}

func TestExecTriageReview_BrokenWorktreeReturnsStaff(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	// Path exists but is not a git repo.
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	out, err := engine.execTriageReview("t1", newTriageStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "staff" {
		t.Errorf("Output = %q, want staff (git error fail-safe)", out.Output)
	}
}

func TestExecTriageReview_TinyDocChangeReturnsSimple(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

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
	if out.Output != "simple" {
		t.Errorf("Output = %q, want simple", out.Output)
	}
}

func TestExecTriageReview_RiskyPathReturnsStaff(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wt := makeGitRepo(t, false)
	// Touch a workflow-internal file — only one line, but risky path.
	risky := filepath.Join(wt, "internal", "workflow")
	if err := os.MkdirAll(risky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(risky, "engine.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", "internal/workflow/engine.go")
	gitRun(t, wt, "commit", "-m", "feat: tweak engine")

	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})

	out, err := engine.execTriageReview("t1", newTriageStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "staff" {
		t.Errorf("Output = %q, want staff (risky path)", out.Output)
	}
}

func TestExecTriageReview_LargeChangeReturnsStaff(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	wt := makeGitRepo(t, false)
	// Write a single non-risky, non-trivial file but with > line limit
	// insertions. Must not be a docs/dep/generated file, or the all-trivial
	// fast-path would deliberately bypass this cap.
	var big strings.Builder
	for range triageReviewLineLimit + 5 {
		big.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(wt, "big.sql"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", "big.sql")
	gitRun(t, wt, "commit", "-m", "chore: big")

	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})

	out, err := engine.execTriageReview("t1", newTriageStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "staff" {
		t.Errorf("Output = %q, want staff (over line limit)", out.Output)
	}
}

// gitRun is a tiny git-exec helper for tests in this file. Mirrors the
// closure inside makeGitRepo so callers can stage post-init commits without
// cracking that helper open.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// --- Builtin pick_review_method routing tests ---

func TestBuiltinSimpleTask_PickReviewMethod(t *testing.T) {
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
		t.Fatal("simple-task-review builtin definition not found")
	}
	step := simple.StepByID("pick_review_method")
	if step == nil {
		t.Fatal("pick_review_method step not found in simple-task-review")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name: "simple_verdict_routes_to_simple",
			fields: map[string]string{
				"task.tags":                      "backend",
				"vars.step.triage_review.output": "simple",
			},
			want: "code_review_simple",
		},
		{
			name: "staff_verdict_still_routes_to_adversarial",
			fields: map[string]string{
				"task.tags":                      "backend",
				"vars.step.triage_review.output": "staff",
			},
			want: "code_review_simple",
		},
		{
			name: "force_staff_tag_is_the_only_route_to_staff",
			fields: map[string]string{
				"task.tags":                      "backend,force-staff-review",
				"vars.step.triage_review.output": "simple",
			},
			want: "code_review_staff",
		},
		{
			name: "force_staff_tag_overrides_staff_verdict_too",
			fields: map[string]string{
				"task.tags":                      "backend,force-staff-review",
				"vars.step.triage_review.output": "staff",
			},
			want: "code_review_staff",
		},
		{
			name: "missing_verdict_falls_back_to_adversarial",
			fields: map[string]string{
				"task.tags": "backend",
			},
			want: "code_review_simple",
		},
		{
			name: "skip_verdict_routes_to_done_review",
			fields: map[string]string{
				"task.tags":                      "backend",
				"vars.step.triage_review.output": "skip",
			},
			want: "done_review",
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

func TestBuiltinPRReview_StaffPassUsesSonnet(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var pr *Definition
	for i := range defs {
		if defs[i].ID == "pr-review" {
			pr = &defs[i]
			break
		}
	}
	if pr == nil {
		t.Fatal("pr-review builtin definition not found")
	}
	step := pr.StepByID("review_staff")
	if step == nil {
		t.Fatal("review_staff step not found in pr-review")
	}
	if step.Config.Model != "sonnet" {
		t.Fatalf("review_staff model = %q, want %q (aligned to simple-task-review's staff pass)", step.Config.Model, "sonnet")
	}
}

func TestBuiltinPRReview_AuthorizesVisibleReviewAction(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var pr *Definition
	for i := range defs {
		if defs[i].ID == "pr-review" {
			pr = &defs[i]
			break
		}
	}
	if pr == nil {
		t.Fatal("pr-review builtin definition not found")
	}
	for _, stepID := range []string{"review_simple", "review_staff"} {
		step := pr.StepByID(stepID)
		if step == nil {
			t.Fatalf("%s step not found in pr-review", stepID)
		}
		for _, want := range []string{
			"Do not ask the operator for confirmation",
			"NEVER submit an approving review",
			"_Generated by Sybra harness_",
		} {
			if !strings.Contains(step.Config.Prompt, want) {
				t.Fatalf("%s prompt missing %q:\n%s", stepID, want, step.Config.Prompt)
			}
		}
		if strings.Contains(step.Config.Prompt, "submit an approve review") {
			t.Fatalf("%s prompt authorizes approval:\n%s", stepID, step.Config.Prompt)
		}
	}
}

func TestBuiltinPRReview_PickReviewMethod(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var pr *Definition
	for i := range defs {
		if defs[i].ID == "pr-review" {
			pr = &defs[i]
			break
		}
	}
	if pr == nil {
		t.Fatal("pr-review builtin definition not found")
	}
	step := pr.StepByID("pick_review_method")
	if step == nil {
		t.Fatal("pick_review_method step not found in pr-review")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name: "simple_verdict_routes_to_simple",
			fields: map[string]string{
				"task.tags":                      "review",
				"vars.step.triage_review.output": "simple",
			},
			want: "review_simple",
		},
		{
			name: "staff_verdict_routes_to_staff",
			fields: map[string]string{
				"task.tags":                      "review",
				"vars.step.triage_review.output": "staff",
			},
			want: "review_staff",
		},
		{
			name: "force_staff_tag_overrides_simple_verdict",
			fields: map[string]string{
				"task.tags":                      "review,force-staff-review",
				"vars.step.triage_review.output": "simple",
			},
			want: "review_staff",
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
