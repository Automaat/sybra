package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyTamperPath(t *testing.T) {
	t.Parallel()
	cases := map[string]tamperCategory{
		"internal/task/parser_test.go":     tamperCatTest,
		"pkg/util/strings_test.go":         tamperCatTest,
		"tests/test_login.py":              tamperCatTest,
		"app/auth_test.py":                 tamperCatTest,
		"frontend/src/App.test.ts":         tamperCatTest,
		"frontend/src/App.spec.tsx":        tamperCatTest,
		"src/__tests__/util.js":            tamperCatTest,
		"spec/models/user_spec.rb":         tamperCatTest,
		"frontend/src/__snapshots__/x.js":  tamperCatSnapshot,
		"internal/foo/testdata/golden.txt": tamperCatSnapshot,
		"cmd/x/cli.snap":                   tamperCatSnapshot,
		"internal/render/out.golden":       tamperCatSnapshot,
		"test/fixtures/payload.json":       tamperCatTest, // under tests dir wins
		"internal/fixtures/payload.json":   tamperCatFixture,
		".github/workflows/ci.yml":         tamperCatCI,
		".gitlab-ci.yml":                   tamperCatCI,
		".sybra.yaml":                      tamperCatCI,
		".golangci.yaml":                   tamperCatCI,
		"Makefile":                         tamperCatCI,
		"mise.toml":                        tamperCatCI,
		"internal/workflow/engine.go":      tamperCatOther,
		"frontend/src/App.svelte":          tamperCatOther,
		"README.md":                        tamperCatOther,
		"internal/task/model.go":           tamperCatOther,
		"contestant/best.go":               tamperCatOther, // "test" substring must not match
	}
	for path, want := range cases {
		if got := classifyTamperPath(path); got != want {
			t.Errorf("classifyTamperPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestScanTamperPatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cat         tamperCategory // defaults to test when empty
		patch       string
		baseContent string
		wantRules   []string // high-severity rules expected (order-insensitive subset)
	}{
		{
			name:      "added_go_skip",
			patch:     "@@ -1,3 +1,4 @@\n func TestFoo(t *testing.T) {\n+\tt.Skip(\"flaky\")\n \tif x != 1 {\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:  "added_skip_matches_established_idiom_elsewhere_in_file",
			patch: "@@ @@\n func TestBar(t *testing.T) {\n+\tif !hasGit() { t.Skip(\"git not available\") }\n",
			baseContent: "func TestFoo(t *testing.T) {\n\tif !hasGit() { t.Skip(\"git not available\") }\n}\n\n" +
				"func TestBar(t *testing.T) {\n}\n",
			wantRules: nil, // identical skip guard already established in the base file — pre-existing idiom
		},
		{
			name:        "added_skip_no_matching_content_line_still_flags",
			patch:       "@@ @@\n func TestBar(t *testing.T) {\n+\tt.Skip(\"flaky\")\n",
			baseContent: "func TestBar(t *testing.T) {\n}\n", // no prior occurrence in the base file
			wantRules:   []string{"added-skip"},
		},
		{
			name:  "two_new_identical_skips_same_commit_still_flags",
			patch: "@@ @@\n func TestFoo(t *testing.T) {\n+\tt.Skip(\"flaky\")\n func TestBar(t *testing.T) {\n+\tt.Skip(\"flaky\")\n",
			baseContent: "func TestFoo(t *testing.T) {\n}\n\n" +
				"func TestBar(t *testing.T) {\n}\n", // neither skip line pre-existed
			wantRules: []string{"added-skip"}, // must not "establish" each other via the post-change file
		},
		{
			name:      "added_pytest_skip",
			patch:     "@@ @@\n+@pytest.mark.skip(reason=\"todo\")\n def test_login():\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "added_jest_skip",
			patch:     "@@ @@\n-it(\"works\", () => {\n+it.skip(\"works\", () => {\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "added_early_exit",
			patch:     "@@ @@\n func TestX(t *testing.T) {\n+\tos.Exit(0)\n",
			wantRules: []string{"added-early-exit"},
		},
		{
			name:      "added_build_ignore",
			patch:     "@@ @@\n+//go:build ignore\n package foo\n",
			wantRules: []string{"added-build-ignore"},
		},
		{
			name:      "removed_assertions_net",
			patch:     "@@ @@\n-\tif x != 1 { t.Errorf(\"a\") }\n-\tif y != 2 { t.Fatalf(\"b\") }\n \tok := true\n",
			wantRules: []string{"removed-assertions"},
		},
		{
			name:      "removed_test_case",
			patch:     "@@ @@\n-func TestGone(t *testing.T) {\n-\tt.Errorf(\"x\")\n-}\n",
			wantRules: []string{"removed-test-cases", "removed-assertions"},
		},
		{
			name:      "assertion_rename_no_net_loss",
			patch:     "@@ @@\n-\trequire.Equal(t, a, b)\n+\trequire.Equal(t, a, c)\n",
			wantRules: nil, // 1 removed, 1 added → net 0, no finding
		},
		{
			name:      "pure_addition_no_finding",
			patch:     "@@ @@\n+func TestNew(t *testing.T) {\n+\trequire.NoError(t, err)\n+}\n",
			wantRules: nil,
		},
		{
			name:      "headers_ignored",
			patch:     "--- a/foo_test.go\n+++ b/foo_test.go\n@@ @@\n context\n",
			wantRules: nil,
		},
		{
			name:      "commented_out_assertion_counts_as_removal",
			patch:     "@@ @@\n-\trequire.NoError(t, err)\n+\t// require.NoError(t, err)\n",
			wantRules: []string{"removed-assertions"},
		},
		{
			name:      "commented_out_test_func_counts_as_removal",
			patch:     "@@ @@\n-func TestFoo(t *testing.T) {\n+// func TestFoo(t *testing.T) {\n",
			wantRules: []string{"removed-test-cases"},
		},
		{
			name:      "tautological_testify_equal",
			patch:     "@@ @@\n-\trequire.Equal(t, want, got)\n+\trequire.Equal(t, got, got)\n",
			wantRules: []string{"tautological-assertion"},
		},
		{
			name:      "tautological_jest_tobe",
			patch:     "@@ @@\n+\texpect(result).toBe(result)\n",
			wantRules: []string{"tautological-assertion"},
		},
		{
			name:      "tautological_python_assert",
			patch:     "@@ @@\n+    assert value == value\n",
			wantRules: []string{"tautological-assertion"},
		},
		{
			name:      "removed_line_starting_with_dashes_not_header",
			patch:     "@@ -1 +0,0 @@\n--- legacy bullet text\n",
			wantRules: nil, // in-hunk content line (not a file header); no tokens → no finding
		},
		{
			name:      "js_require_import_removal_is_not_an_assertion",
			patch:     "@@ @@\n-const dep = require(\"dep\")\n",
			wantRules: nil, // bare require is an import, not an assertion
		},
		{
			name:      "fmt_errorf_is_not_an_assertion",
			patch:     "@@ @@\n-\treturn fmt.Errorf(\"bad: %w\", err)\n",
			wantRules: nil,
		},
		{
			name:      "ci_removed_test_step",
			cat:       tamperCatCI,
			patch:     "@@ @@\n       - run: go vet ./...\n-      - run: go test ./...\n",
			wantRules: []string{"removed-ci-step"},
		},
		{
			name:      "ci_neuter_token",
			cat:       tamperCatCI,
			patch:     "@@ @@\n+    continue-on-error: true\n",
			wantRules: []string{"ci-neutered"},
		},
		{
			name:      "ci_renamed_step_no_net_loss",
			cat:       tamperCatCI,
			patch:     "@@ @@\n-      - run: go test ./...\n+      - run: go test ./... -race\n",
			wantRules: nil, // 1 removed, 1 re-added → net 0
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat := tc.cat
			if cat == "" {
				cat = tamperCatTest
			}
			got := scanTamperPatch("x_test.go", cat, tc.patch, tc.baseContent)
			gotRules := map[string]bool{}
			for _, f := range got {
				if f.Severity != tamperHigh {
					t.Errorf("finding %q has severity %q, want high", f.Rule, f.Severity)
				}
				gotRules[f.Rule] = true
			}
			for _, want := range tc.wantRules {
				if !gotRules[want] {
					t.Errorf("missing expected rule %q; got %v", want, gotRules)
				}
			}
			if tc.wantRules == nil && len(got) != 0 {
				t.Errorf("expected no findings, got %v", got)
			}
		})
	}
}

func TestBuildTamperReport(t *testing.T) {
	t.Parallel()

	t.Run("deleted_test_file_is_high", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/foo/foo_test.go"},
		})
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
		if r.Findings[0].Rule != "deleted-verification-file" {
			t.Errorf("rule = %q, want deleted-verification-file", r.Findings[0].Rule)
		}
	})

	t.Run("impl_only_change_no_files", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "M", Path: "internal/foo/foo.go", Patch: "@@ @@\n+x := 1\n"},
		})
		if len(r.Files) != 0 {
			t.Errorf("Files = %v, want empty (non-verification change)", r.Files)
		}
		if len(r.Findings) != 0 {
			t.Errorf("Findings = %v, want none", r.Findings)
		}
	})

	t.Run("benign_test_change_is_medium_not_blocking", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "A", Path: "internal/foo/new_test.go", Patch: "@@ @@\n+func TestNew(t *testing.T){ require.NoError(t, err) }\n"},
		})
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Files) != 1 {
			t.Errorf("Files = %v, want the changed test file recorded", r.Files)
		}
		if len(r.Findings) != 1 || r.Findings[0].Severity != tamperMedium {
			t.Errorf("want one medium finding, got %v", r.Findings)
		}
	})

	t.Run("assertion_refactor_is_medium_not_blocking", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "M", Path: "internal/foo/old_test.go", Patch: "@@ @@\n-\tif err != nil { t.Fatalf(\"bad: %v\", err) }\n"},
			{Status: "A", Path: "internal/foo/helpers_test.go", Patch: "@@ @@\n+func mustOK(tb testing.TB, err error) {\n+\tif err != nil { tb.Fatalf(\"bad: %v\", err) }\n+}\n"},
		})
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Findings) != 1 || r.Findings[0].Rule != "removed-assertions" || r.Findings[0].Severity != tamperMedium {
			t.Fatalf("want removed-assertions downgraded to medium, got %v", r.Findings)
		}
	})

	t.Run("tampering_mixes_high_and_skips_medium", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "M", Path: "a_test.go", Patch: "@@ @@\n+\tt.Skip(\"x\")\n"},
			{Status: "M", Path: "b_test.go", Patch: "@@ @@\n+\tfoo := 1\n"},
		})
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
		// When a high finding fires, medium fallbacks are suppressed.
		for _, f := range r.Findings {
			if f.Severity == tamperMedium {
				t.Errorf("unexpected medium finding alongside high: %v", f)
			}
		}
	})
}

func TestParseNameStatus(t *testing.T) {
	t.Parallel()
	out := "M\tinternal/foo/foo.go\nD\tinternal/foo/foo_test.go\nR100\told/x_test.go\tnew/x_test.go\n\nA\tbar.go\n"
	got := parseNameStatus(out)
	want := []tamperChange{
		{Status: "M", Path: "internal/foo/foo.go"},
		{Status: "D", Path: "internal/foo/foo_test.go"},
		{Status: "R100", Path: "new/x_test.go"},
		{Status: "A", Path: "bar.go"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// --- execDetectTampering integration tests (real git) ---

func newTamperStep() *Step { return &Step{ID: "detect_tampering", Type: StepDetectTampering} }

func TestBuiltinSimpleTaskImplement_DetectTamperingWiring(t *testing.T) {
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
		t.Fatal("simple-task-implement builtin not found")
	}

	tamper := impl.StepByID("detect_tampering")
	if tamper == nil {
		t.Fatal("detect_tampering step missing from simple-task-implement")
	}
	if tamper.Type != StepDetectTampering {
		t.Errorf("detect_tampering type = %q, want %q", tamper.Type, StepDetectTampering)
	}

	// verify_commits default (no status condition) must route to detect_tampering.
	vc := impl.StepByID("verify_commits")
	if vc == nil {
		t.Fatal("verify_commits step missing")
	}
	if got, _ := ResolveTransition(vc.Next, map[string]string{"task.status": "ready-review"}); got != "detect_tampering" {
		t.Errorf("verify_commits default goto = %q, want detect_tampering", got)
	}

	cases := []struct {
		name   string
		status string
		want   string
	}{
		{"flagged_ends_workflow", "human-required", ""},
		{"clean_flows_to_verify_checks", "ready-review", "verify_checks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(tamper.Next, map[string]string{"task.status": tc.status})
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

func writeRepoFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// makeBaseRepo inits a repo with baseFiles committed as the origin/main state.
func makeBaseRepo(t *testing.T, baseFiles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "config", "user.email", "test@test.com")
	gitRun(t, dir, "config", "user.name", "Test")
	for path, content := range baseFiles {
		writeRepoFile(t, dir, path, content)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "init")
	gitRun(t, dir, "remote", "add", "origin", dir)
	gitRun(t, dir, "fetch", "origin")
	return dir
}

func newTamperEngine(t *testing.T, wt string) (*Engine, *memTasks) {
	t.Helper()
	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	return engine, tasks
}

func TestExecDetectTampering_NoWorktreeSkips(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output == "flagged" {
		t.Errorf("no worktree must not flag; got %q", out.Output)
	}
}

func TestExecDetectTampering_CleanImplChange(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	writeRepoFile(t, wt, "internal/foo/foo.go", "package foo\n\nfunc Foo() int { return 1 }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: add foo")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Errorf("Output = %q, want clean", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
	}
}

func TestExecDetectTampering_AddedSkipFlags(t *testing.T) {
	t.Parallel()
	base := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Foo() int { return 1 }\n",
		"internal/foo/foo_test.go": base,
	})
	// Neuter the test with a skip.
	tampered := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tt.Skip(\"flaky\")\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	writeRepoFile(t, wt, "internal/foo/foo_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: skip foo")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); reason == "" {
		t.Errorf("expected a non-empty status reason")
	}
}

func TestExecDetectTampering_EstablishedSkipIdiomDoesNotFlag(t *testing.T) {
	t.Parallel()
	skipLine := "\tif !hasGit() { t.Skip(\"git not available\") }"
	base := "package foo\n\nimport \"testing\"\n\nfunc hasGit() bool { return true }\n\n" +
		"func TestFoo(t *testing.T) {\n" + skipLine + "\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Foo() int { return 1 }\n",
		"internal/foo/foo_test.go": base,
	})
	// New test follows the exact same, pre-existing hasGit() skip guard idiom
	// already used by TestFoo — this is an established repo convention, not a
	// novel tampering attempt, so it must not flag.
	tampered := base + "\nfunc TestBar(t *testing.T) {\n" + skipLine +
		"\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	writeRepoFile(t, wt, "internal/foo/foo_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: add bar with established skip guard")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output == "flagged" {
		t.Fatalf("Output = %q, want not flagged (established skip idiom)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
	}
}

func TestExecDetectTampering_TwoNewIdenticalSkipsSameCommitStillFlags(t *testing.T) {
	t.Parallel()
	base := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n\n" +
		"func TestBar(t *testing.T) {\n\tif Foo() != 2 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Foo() int { return 1 }\n",
		"internal/foo/foo_test.go": base,
	})
	// Two brand-new, identical skip lines added in the same commit, with zero
	// prior occurrence in the base file — must not "establish" each other.
	tampered := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tt.Skip(\"flaky\")\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n\n" +
		"func TestBar(t *testing.T) {\n\tt.Skip(\"flaky\")\n\tif Foo() != 2 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	writeRepoFile(t, wt, "internal/foo/foo_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: skip foo and bar")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged (two novel identical skips must not bless each other)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecDetectTampering_DeletedTestFlags(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo_test.go": "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) { t.Errorf(\"x\") }\n",
	})
	gitRun(t, wt, "rm", "internal/foo/foo_test.go")
	gitRun(t, wt, "commit", "-m", "chore: drop test")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecDetectTampering_BlessedTagShortCircuits(t *testing.T) {
	t.Parallel()
	base := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Foo() int { return 1 }\n",
		"internal/foo/foo_test.go": base,
	})
	// Same tampering as the AddedSkip test, but the human has blessed it.
	tampered := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tt.Skip(\"flaky\")\n}\n"
	writeRepoFile(t, wt, "internal/foo/foo_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: skip foo")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(),
		TaskInfo{ID: "t1", Tags: []string{"tamper-blessed"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "blessed" {
		t.Errorf("Output = %q, want blessed (tag short-circuits the scan)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged (no block when blessed)", ti.Status)
	}
}

func TestExecDetectTampering_CommentedOutTestFlags(t *testing.T) {
	t.Parallel()
	base := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\trequire.NoError(t, doThing())\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo_test.go": base,
	})
	// Comment out the assertion instead of fixing the code.
	tampered := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\t// require.NoError(t, doThing())\n}\n"
	writeRepoFile(t, wt, "internal/foo/foo_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: comment out")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged (commenting out an assertion is tampering)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecDetectTampering_NeuteredCIFlags(t *testing.T) {
	t.Parallel()
	base := "name: ci\njobs:\n  test:\n    steps:\n      - run: go test ./...\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		".github/workflows/ci.yml": base,
	})
	tampered := "name: ci\njobs:\n  test:\n    continue-on-error: true\n    steps:\n      - run: go test ./...\n"
	writeRepoFile(t, wt, ".github/workflows/ci.yml", tampered)
	gitRun(t, wt, "commit", "-am", "ci: neuter")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged (continue-on-error neuters the gate)", out.Output)
	}
}

func TestExecDetectTampering_UsesAgentBaseline(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                  "init\n",
		"frontend/Dashboard.test.ts": "test('dashboard', () => expect(true).toBe(true))\n",
	})
	gitRun(t, wt, "rm", "frontend/Dashboard.test.ts")
	gitRun(t, wt, "commit", "-m", "refactor: remove dashboard")
	baseline := strings.TrimSpace(gitOutput(t, wt, "rev-parse", "HEAD"))
	writeRepoFile(t, wt, "internal/foo/foo.go", "package foo\n\nfunc Foo() int { return 1 }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: add foo")

	engine, tasks := newTamperEngine(t, wt)
	wf := &Execution{
		Variables: map[string]string{tamperBaselineVar("fix"): baseline},
		StepHistory: []StepRecord{{
			StepID:  "fix",
			Status:  "completed",
			AgentID: "agent-1",
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1", Workflow: wf})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean; baseline should ignore stale test deletion", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
	}
}

func TestBuiltinPRFix_DetectTamperingWiring(t *testing.T) {
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
		t.Fatal("pr-fix builtin not found")
	}
	if prfix.StepByID("detect_tampering") == nil {
		t.Fatal("detect_tampering step missing from pr-fix")
	}
	vc := prfix.StepByID("verify_commits")
	if vc == nil {
		t.Fatal("verify_commits step missing from pr-fix")
	}
	if got, _ := ResolveTransition(vc.Next, map[string]string{"task.status": "in-progress"}); got != "detect_tampering" {
		t.Errorf("pr-fix verify_commits default goto = %q, want detect_tampering", got)
	}
}

func TestBuiltinSimpleTaskReview_DetectTamperingWiring(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var rev *Definition
	for i := range defs {
		if defs[i].ID == "simple-task-review" {
			rev = &defs[i]
			break
		}
	}
	if rev == nil {
		t.Fatal("simple-task-review builtin not found")
	}
	tamper := rev.StepByID("detect_tampering")
	if tamper == nil {
		t.Fatal("detect_tampering step missing from simple-task-review")
	}
	fix := rev.StepByID("fix_review")
	if fix == nil {
		t.Fatal("fix_review step missing from simple-task-review")
	}
	if got, _ := ResolveTransition(fix.Next, map[string]string{"task.status": "ready-review"}); got != "detect_tampering" {
		t.Errorf("fix_review goto = %q, want detect_tampering", got)
	}
	if got, _ := ResolveTransition(tamper.Next, map[string]string{"task.status": "human-required"}); got != "" {
		t.Errorf("flagged detect_tampering goto = %q, want end", got)
	}
}

func TestExecDetectTampering_BenignTestAddDoesNotBlock(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n", "internal/foo/foo.go": "package foo\n\nfunc Foo() int { return 1 }\n"})
	writeRepoFile(t, wt, "internal/foo/foo_test.go",
		"package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "test: add coverage")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Errorf("Output = %q, want clean (adding tests is not tampering)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
	}
}
