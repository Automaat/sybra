package workflow

import (
	"context"
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
		name            string
		cat             tamperCategory // defaults to test when empty
		patch           string
		baseContent     string
		upstreamContent string
		wantRules       []string // high-severity rules expected (order-insensitive subset)
	}{
		{
			name:      "added_go_skip",
			patch:     "@@ -1,3 +1,4 @@\n func TestFoo(t *testing.T) {\n+\tt.Skip(\"flaky\")\n \tif x != 1 {\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "go_skip_in_interpreted_fixture_string_not_flagged",
			patch:     "@@ @@\n func TestDetector(t *testing.T) {\n+\tpatch := \"@@ @@\\n+\\tt.Skip(\\\"linux-only\\\")\\n\"\n+\t_ = patch\n }\n",
			wantRules: nil,
		},
		{
			name:      "go_skip_in_raw_fixture_string_not_flagged",
			patch:     "@@ @@\n func TestDetector(t *testing.T) {\n+\tpatch := `@@ @@\n++\tt.Skip(\"linux-only\")\n+`\n+\t_ = patch\n }\n",
			wantRules: nil,
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
			name:            "added_skip_merged_from_upstream_not_flagged",
			patch:           "@@ @@\n func TestBar(t *testing.T) {\n+\tt.Skip(\"flaky\")\n+\tt.Fatal(\"updated\")\n",
			baseContent:     "func TestBar(t *testing.T) {\n\tif ready {\n\t\tt.Fatal(\"x\")\n\t}\n}\n",
			upstreamContent: "func TestBar(t *testing.T) {\n\tif ready {\n\t\tt.Skip(\"flaky\")\n\t\tt.Fatal(\"x\")\n\t}\n}\n",
			wantRules:       nil,
		},
		{
			name:            "extra_identical_skip_beyond_upstream_delta_still_flags",
			patch:           "@@ @@\n func TestBar(t *testing.T) {\n+\tt.Skip(\"flaky\")\n+\tt.Skip(\"flaky\")\n",
			baseContent:     "func TestBar(t *testing.T) {\n}\n",
			upstreamContent: "func TestBar(t *testing.T) {\n\tt.Skip(\"flaky\")\n}\n",
			wantRules:       []string{"added-skip"},
		},
		{
			name:      "capability_guarded_skip_same_line_not_flagged",
			patch:     "@@ @@\n func TestSymlink(t *testing.T) {\n+\tif err := os.Symlink(a, b); err != nil { t.Skipf(\"symlink unsupported: %v\", err) }\n",
			wantRules: nil,
		},
		{
			name:      "capability_guarded_skip_next_line_not_flagged",
			patch:     "@@ @@\n func TestSymlink(t *testing.T) {\n+\tif err := os.Symlink(a, b); err != nil {\n+\t\tt.Skipf(\"symlink unsupported: %v\", err)\n+\t}\n",
			wantRules: nil,
		},
		{
			name:      "testing_short_guarded_skip_not_flagged",
			patch:     "@@ @@\n func TestSlow(t *testing.T) {\n+\tif testing.Short() {\n+\t\tt.Skip(\"skipping slow test in -short\")\n+\t}\n",
			wantRules: nil,
		},
		{
			name:      "lookpath_guarded_skip_not_flagged",
			patch:     "@@ @@\n func TestDocker(t *testing.T) {\n+\tif _, err := exec.LookPath(\"docker\"); err != nil {\n+\t\tt.Skip(\"docker not installed\")\n+\t}\n",
			wantRules: nil,
		},
		{
			name: "guard_window_resets_between_hunks",
			patch: "@@ @@\n func TestDocker(t *testing.T) {\n+\tif _, err := exec.LookPath(\"docker\"); err != nil {\n+\t\tt.Skip(\"docker not installed\")\n+\t}\n" +
				"@@ @@\n func TestFlaky(t *testing.T) {\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
		},
		{
			name: "guard_window_resets_between_files",
			patch: "diff --git a/a_test.go b/a_test.go\n@@ @@\n func TestDocker(t *testing.T) {\n+\tif _, err := exec.LookPath(\"docker\"); err != nil {\n+\t\tt.Skip(\"docker not installed\")\n+\t}\n" +
				"diff --git a/b_test.go b/b_test.go\n@@ @@\n func TestFlaky(t *testing.T) {\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "goos_guarded_skip_not_flagged",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != \"linux\" {\n+\t\tt.Skip(\"linux-only process enumeration test\")\n+\t}\n",
			wantRules: nil,
		},
		{
			name:      "goarch_guarded_skip_not_flagged",
			patch:     "@@ @@\n func TestARM(t *testing.T) {\n+\tif runtime.GOARCH != \"arm64\" {\n+\t\tt.Skip(\"arm64-only test\")\n+\t}\n",
			wantRules: nil,
		},
		{
			name:      "composite_platform_guarded_skip_not_flagged",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != \"linux\" || runtime.GOARCH != \"amd64\" {\n+\t\tt.Skip(\"linux/amd64-only test\")\n+\t}\n",
			wantRules: nil,
		},
		{
			name:      "unknown_goos_guarded_skip_still_flags",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != \"not-a-real-os\" {\n+\t\tt.Skip(\"flaky\")\n+\t}\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "identifier_goos_guarded_skip_still_flags",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != targetOS {\n+\t\tt.Skip(\"flaky\")\n+\t}\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "mixed_platform_and_nonplatform_guard_still_flags",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != \"linux\" || flakyEnvironment() {\n+\t\tt.Skip(\"flaky\")\n+\t}\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "unconditional_skip_after_platform_guard_still_flags",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != \"linux\" {\n+\t\tsetup()\n+\t}\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "unrelated_skip_after_platform_guard_closed_by_context_still_flags",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n \t{\n-\t\tif setupOK() {\n+\t\tif runtime.GOOS != \"linux\" { if setupOK() {\n \t\t\tsetup()\n \t\t}\n \t}\n }\n \n func TestOther(t *testing.T) {\n+\tt.Skip(\"unrelated flaky\")\n \tassertReady(t)\n }\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "skip_message_brace_does_not_extend_platform_guard",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != \"linux\" {\n+\t\tt.Skip(\"only works on {legacy} platform\")\n+\t}\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "bare_goos_reference_does_not_guard_later_skip",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tplatform := runtime.GOOS\n+\t_ = platform\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "bare_goarch_reference_does_not_guard_later_skip",
			patch:     "@@ @@\n func TestARM(t *testing.T) {\n+\tarch := runtime.GOARCH\n+\t_ = arch\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "unconditional_skip_after_guard_window_still_flags",
			patch:     "@@ @@\n func TestX(t *testing.T) {\n+\tif _, err := exec.LookPath(\"docker\"); err != nil {\n+\t\treturn\n+\t}\n+\tsetup()\n+\tvalidate()\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "platform_guarded_skip_same_line_not_flagged",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != \"linux\" { t.Skip(\"linux-only\") }\n",
			wantRules: nil,
		},
		{
			name:      "platform_guarded_skip_next_line_not_flagged",
			patch:     "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOARCH != \"amd64\" {\n+\t\tt.Skip(\"amd64-only\")\n+\t}\n",
			wantRules: nil,
		},
		{
			// Self-hosted false positive (issue #2323): a skip pattern that
			// only exists inside a Go string literal — e.g. a diff fixture
			// embedded in this detector's own regression tests — must not
			// be mistaken for a real added t.Skip call. The added line here
			// is the on-disk source text of a _test.go file whose *value*
			// happens to contain an escaped `t.Skip(...)` sequence; the
			// whole thing sits inside one unbroken, still-open string
			// literal (escaped inner quotes never close it).
			name: "skip_pattern_inside_go_string_literal_not_flagged",
			patch: "@@ @@\n func TestFixture(t *testing.T) {\n" +
				"+\tpatch := \"@@ @@\\n func TestReap(t *testing.T) {\\n+\\tif runtime.GOOS != \\\"linux\\\" { t.Skip(\\\"linux-only\\\") }\\n\"\n",
			wantRules: nil,
		},
		{
			// Same skip pattern, but as real unquoted code (no fixture
			// wrapping) — must still flag. Guards against an overbroad
			// masking fix silently swallowing genuine tampering.
			name:      "skip_pattern_outside_go_string_literal_still_flags",
			patch:     "@@ @@\n func TestFixture(t *testing.T) {\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
		},
		{
			name:      "guard_does_not_leak_across_hunks",
			patch:     "@@ @@\n func TestGuarded(t *testing.T) {\n+\tif _, err := exec.LookPath(\"docker\"); err != nil {\n@@ @@\n func TestOther(t *testing.T) {\n+\tt.Skip(\"flaky\")\n",
			wantRules: []string{"added-skip"},
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
			got := scanTamperPatch("x_test.go", cat, tc.patch, tc.baseContent, tc.upstreamContent)
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
		}, tamperDeletionAllowlist{})
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
		}, tamperDeletionAllowlist{})
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
		}, tamperDeletionAllowlist{})
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
		}, tamperDeletionAllowlist{})
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Findings) != 1 || r.Findings[0].Rule != "removed-assertions" || r.Findings[0].Severity != tamperMedium {
			t.Fatalf("want removed-assertions downgraded to medium, got %v", r.Findings)
		}
	})

	t.Run("test_case_moved_across_files_is_medium_not_blocking", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "M", Path: "internal/foo/old_test.go", Patch: "@@ @@\n-func TestGone(t *testing.T) {\n-\tt.Errorf(\"x\")\n-}\n"},
			{Status: "A", Path: "internal/foo/new_test.go", Patch: "@@ @@\n+func TestGone(t *testing.T) {\n+\tt.Errorf(\"x\")\n+}\n+func TestExtra(t *testing.T) {\n+\tt.Errorf(\"y\")\n+}\n"},
		}, tamperDeletionAllowlist{})
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (consolidation is diff-wide net-neutral): %v", r.highCount(), r.Findings)
		}
		foundDecl := false
		for _, f := range r.Findings {
			if f.Rule == "removed-test-cases" {
				foundDecl = true
				if f.Severity != tamperMedium {
					t.Errorf("removed-test-cases severity = %q, want medium", f.Severity)
				}
			}
		}
		if !foundDecl {
			t.Fatalf("want a removed-test-cases finding (downgraded), got %v", r.Findings)
		}
	})

	t.Run("test_case_removed_with_no_offsetting_addition_stays_high", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "M", Path: "internal/foo/old_test.go", Patch: "@@ @@\n-func TestGone(t *testing.T) {\n-\tt.Errorf(\"x\")\n-}\n"},
			{Status: "M", Path: "internal/foo/other_test.go", Patch: "@@ @@\n+\tfoo := 1\n"},
		}, tamperDeletionAllowlist{})
		found := false
		for _, f := range r.Findings {
			if f.Rule == "removed-test-cases" {
				found = true
				if f.Severity != tamperHigh {
					t.Errorf("removed-test-cases severity = %q, want high (no offsetting addition anywhere in diff)", f.Severity)
				}
			}
		}
		if !found {
			t.Fatalf("want a removed-test-cases finding, got %v", r.Findings)
		}
	})

	t.Run("removed_assertions_with_unrelated_addition_stays_high", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "M", Path: "internal/foo/old_test.go", Patch: "@@ @@\n-\tif err != nil { t.Fatalf(\"bad: %v\", err) }\n-\tif y != 2 { t.Fatalf(\"bad y\") }\n"},
			{Status: "A", Path: "internal/foo/new_feature_test.go", Patch: "@@ @@\n+func TestUnrelated(t *testing.T) {\n+\trequire.NoError(t, err)\n+}\n"},
		}, tamperDeletionAllowlist{})
		found := false
		for _, f := range r.Findings {
			if f.Rule == "removed-assertions" {
				found = true
				if f.Severity != tamperHigh {
					t.Errorf("removed-assertions severity = %q, want high (only 1 of 2 removed assertions offset)", f.Severity)
				}
			}
		}
		if !found {
			t.Fatalf("want a removed-assertions finding, got %v", r.Findings)
		}
	})

	t.Run("tampering_mixes_high_and_skips_medium", func(t *testing.T) {
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "M", Path: "a_test.go", Patch: "@@ @@\n+\tt.Skip(\"x\")\n"},
			{Status: "M", Path: "b_test.go", Patch: "@@ @@\n+\tfoo := 1\n"},
		}, tamperDeletionAllowlist{})
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

	t.Run("documented_deleted_test_file_is_downgraded", func(t *testing.T) {
		body := "## Scope\n- delete mesh_helpers_test.go\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/mesh/mesh_helpers_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Findings) != 1 {
			t.Fatalf("Findings = %v, want 1 downgraded deletion finding", r.Findings)
		}
		if r.Findings[0].Rule != "deleted-verification-file" || r.Findings[0].Severity != tamperMedium {
			t.Fatalf("finding = %+v, want deleted-verification-file downgraded to medium", r.Findings[0])
		}
		if !strings.Contains(r.Findings[0].Detail, "documented in task spec") {
			t.Fatalf("detail = %q, want documented marker", r.Findings[0].Detail)
		}
	})

	t.Run("plan_contract_expected_exact_deletion_is_downgraded", func(t *testing.T) {
		allow := documentedDeletionAllowlistForTrustedSpec(TaskInfo{
			PlanContract: strings.Replace(validPlanContract("t1"),
				`  "verification": [`,
				`  "expected_deletions": ["internal/mesh/mesh_helpers_test.go"],
  "verification": [`, 1),
		})
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/mesh/mesh_helpers_test.go"},
		}, allow)
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Findings) != 1 {
			t.Fatalf("Findings = %v, want 1 downgraded deletion finding", r.Findings)
		}
		if got := r.Findings[0].Detail; !strings.Contains(got, "expected_deletions") {
			t.Fatalf("detail = %q, want expected_deletions marker", got)
		}
	})

	t.Run("plan_contract_expected_glob_deletion_is_downgraded", func(t *testing.T) {
		allow := documentedDeletionAllowlistForTrustedSpec(TaskInfo{
			PlanContract: strings.Replace(validPlanContract("t1"),
				`  "verification": [`,
				`  "expected_deletions": ["app/foo/testdata/*.golden"],
  "verification": [`, 1),
		})
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "app/foo/testdata/get-circuit-breaker.golden"},
		}, allow)
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Findings) != 1 {
			t.Fatalf("Findings = %v, want 1 downgraded deletion finding", r.Findings)
		}
		if got := r.Findings[0].Detail; !strings.Contains(got, "expected_deletions") {
			t.Fatalf("detail = %q, want expected_deletions marker", got)
		}
	})

	t.Run("documented_delete_cases_from_test_file_is_downgraded", func(t *testing.T) {
		body := "## Scope\n- delete mode cases from mesh_helpers_test.go\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/mesh/mesh_helpers_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Findings) != 1 {
			t.Fatalf("Findings = %v, want 1 downgraded deletion finding", r.Findings)
		}
		if r.Findings[0].Rule != "deleted-verification-file" || r.Findings[0].Severity != tamperMedium {
			t.Fatalf("finding = %+v, want deleted-verification-file downgraded to medium", r.Findings[0])
		}
		if !strings.Contains(r.Findings[0].Detail, "documented in task spec") {
			t.Fatalf("detail = %q, want documented marker", r.Findings[0].Detail)
		}
	})

	t.Run("negated_delete_instruction_keeps_deleted_test_file_high", func(t *testing.T) {
		body := "## Scope\n- Do not delete `internal/foo/bar_test.go` under any circumstances\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/foo/bar_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Findings) != 1 {
			t.Fatalf("Findings = %v, want 1 high deletion finding", r.Findings)
		}
		if r.Findings[0].Rule != "deleted-verification-file" || r.Findings[0].Severity != tamperHigh {
			t.Fatalf("finding = %+v, want deleted-verification-file high", r.Findings[0])
		}
		if strings.Contains(r.Findings[0].Detail, "documented in task spec") {
			t.Fatalf("detail = %q, did not want documented marker", r.Findings[0].Detail)
		}
	})

	t.Run("documented_multiple_deleted_test_files_are_downgraded", func(t *testing.T) {
		body := "## Files\n- delete `internal/foo/a_test.go` and `internal/foo/b_test.go`\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/foo/a_test.go"},
			{Status: "D", Path: "internal/foo/b_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 0 {
			t.Fatalf("highCount = %d, want 0 (%v)", r.highCount(), r.Findings)
		}
		if len(r.Findings) != 2 {
			t.Fatalf("Findings = %v, want 2 downgraded deletion findings", r.Findings)
		}
		for _, finding := range r.Findings {
			if finding.Rule != "deleted-verification-file" || finding.Severity != tamperMedium {
				t.Fatalf("finding = %+v, want deleted-verification-file downgraded to medium", finding)
			}
		}
	})

	t.Run("documented_exact_path_does_not_bless_same_basename", func(t *testing.T) {
		body := "## Files\n- delete `internal/ui/helpers_test.go`\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/api/helpers_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("mixed_action_line_does_not_bless_updated_test_file", func(t *testing.T) {
		body := "## Files\n- remove old helper and update `internal/foo/helpers_test.go`\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/foo/helpers_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("mixed_action_line_does_not_bless_touched_test_file", func(t *testing.T) {
		body := "## Files\n- remove old helper and touch `internal/foo/helpers_test.go`\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/foo/helpers_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("earlier_unverbed_path_is_not_blessed_by_later_delete", func(t *testing.T) {
		body := "## Files\n- This task touches `internal/config/loader_test.go` and also needs us to delete `internal/foo/old_test.go`.\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/config/loader_test.go"},
			{Status: "D", Path: "internal/foo/old_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("mixed_action_line_still_blesses_deleted_path", func(t *testing.T) {
		body := "## Files\n- delete `internal/foo/legacy_test.go` and update `internal/foo/helpers_test.go`\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/foo/legacy_test.go"},
			{Status: "D", Path: "internal/foo/helpers_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("non_deletion_mentions_do_not_bless_deleted_test_file", func(t *testing.T) {
		body := "## Scope\n- inspect mesh_helpers_test.go while fixing runtime logic\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/mesh/mesh_helpers_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("remove_inside_file_phrase_does_not_bless_deleted_test_file", func(t *testing.T) {
		body := "## Scope\n- Remove debug logging in foo_test.go\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "foo_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("remove_import_from_file_phrase_does_not_bless_deleted_test_file", func(t *testing.T) {
		body := "## Scope\n- Remove unused import from foo_test.go\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "foo_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("distant_prior_delete_does_not_bless_test_file", func(t *testing.T) {
		body := "## Scope\n- delete the old cache layer entirely, and make sure `internal/foo/bar_test.go` still passes\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/foo/bar_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})

	t.Run("prior_delete_clause_does_not_bless_ensured_test_file", func(t *testing.T) {
		body := "## Scope\n- delete cache and ensure `internal/foo/bar_test.go` still passes\n"
		r := buildTamperReport("t1", "origin/main", []tamperChange{
			{Status: "D", Path: "internal/foo/bar_test.go"},
		}, documentedDeletionAllowlist(body))
		if r.highCount() != 1 {
			t.Fatalf("highCount = %d, want 1 (%v)", r.highCount(), r.Findings)
		}
	})
}

func TestDocumentedDeletionAllowlist(t *testing.T) {
	t.Parallel()

	t.Run("extracts_from_scope_and_files_sections", func(t *testing.T) {
		body := "" +
			"## Scope\n" +
			"- delete mesh_helpers_test.go\n\n" +
			"## Files\n" +
			"- `internal/mesh/legacy_test.go` - remove tests for deleted helper\n"
		got := documentedDeletionAllowlist(body)
		if !got.ExactPaths["mesh_helpers_test.go"] {
			t.Fatalf("ExactPaths = %v, want mesh_helpers_test.go", got.ExactPaths)
		}
		if !got.ExactPaths["internal/mesh/legacy_test.go"] {
			t.Fatalf("ExactPaths = %v, want internal/mesh/legacy_test.go", got.ExactPaths)
		}
		if !got.Basenames["mesh_helpers_test.go"] {
			t.Fatalf("Basenames = %v, want extracted basename-only token", got.Basenames)
		}
		if got.Basenames["legacy_test.go"] {
			t.Fatalf("Basenames = %v, did not want basename from exact path", got.Basenames)
		}
	})

	t.Run("extracts_multiple_paths_from_one_deletion_segment", func(t *testing.T) {
		body := "## Files\n- delete `a_test.go` and `b_test.go`, plus internal/foo/c_test.go\n"
		got := documentedDeletionAllowlist(body)
		for _, path := range []string{"a_test.go", "b_test.go", "internal/foo/c_test.go"} {
			if !got.ExactPaths[path] {
				t.Fatalf("ExactPaths = %v, want %s", got.ExactPaths, path)
			}
		}
		for _, base := range []string{"a_test.go", "b_test.go"} {
			if !got.Basenames[base] {
				t.Fatalf("Basenames = %v, want %s", got.Basenames, base)
			}
		}
		if got.Basenames["c_test.go"] {
			t.Fatalf("Basenames = %v, did not want basename from exact path", got.Basenames)
		}
	})

	t.Run("extracts_delete_cases_from_file_phrase", func(t *testing.T) {
		body := "## Scope\n- delete mode cases from mesh_helpers_test.go\n"
		got := documentedDeletionAllowlist(body)
		if !got.ExactPaths["mesh_helpers_test.go"] {
			t.Fatalf("ExactPaths = %v, want mesh_helpers_test.go", got.ExactPaths)
		}
		if !got.Basenames["mesh_helpers_test.go"] {
			t.Fatalf("Basenames = %v, want mesh_helpers_test.go", got.Basenames)
		}
	})

	t.Run("ignores_unverbed_path_before_later_delete", func(t *testing.T) {
		body := "## Files\n- This task touches `internal/config/loader.go` and also needs us to delete `internal/foo/old_test.go`.\n"
		got := documentedDeletionAllowlist(body)
		if got.ExactPaths["internal/config/loader.go"] {
			t.Fatalf("ExactPaths = %v, did not want unrelated earlier path", got.ExactPaths)
		}
		if !got.ExactPaths["internal/foo/old_test.go"] {
			t.Fatalf("ExactPaths = %v, want explicitly deleted path", got.ExactPaths)
		}
	})

	t.Run("allows_same_path_when_later_explicitly_deleted", func(t *testing.T) {
		body := "## Files\n- Touch `internal/config/loader.go` first, then delete `internal/config/loader.go`.\n"
		got := documentedDeletionAllowlist(body)
		if !got.ExactPaths["internal/config/loader.go"] {
			t.Fatalf("ExactPaths = %v, want later explicit delete of same path", got.ExactPaths)
		}
	})

	t.Run("ignores_path_after_distant_prior_delete", func(t *testing.T) {
		body := "## Files\n- delete the old cache layer entirely, and make sure `internal/foo/bar_test.go` still passes.\n"
		got := documentedDeletionAllowlist(body)
		if got.ExactPaths["internal/foo/bar_test.go"] {
			t.Fatalf("ExactPaths = %v, did not want unrelated test path", got.ExactPaths)
		}
	})

	t.Run("ignores_path_after_short_prior_delete_clause", func(t *testing.T) {
		body := "## Files\n- delete cache and ensure `internal/foo/bar_test.go` still passes.\n"
		got := documentedDeletionAllowlist(body)
		if got.ExactPaths["internal/foo/bar_test.go"] {
			t.Fatalf("ExactPaths = %v, did not want unrelated ensured test path", got.ExactPaths)
		}
	})

	t.Run("keeps_comma_separated_deletion_list", func(t *testing.T) {
		body := "## Files\n- delete `a_test.go` and `b_test.go`, plus internal/foo/c_test.go\n"
		got := documentedDeletionAllowlist(body)
		for _, path := range []string{"a_test.go", "b_test.go", "internal/foo/c_test.go"} {
			if !got.ExactPaths[path] {
				t.Fatalf("ExactPaths = %v, want %s", got.ExactPaths, path)
			}
		}
	})

	t.Run("ignores_fenced_examples", func(t *testing.T) {
		body := "" +
			"## Scope\n" +
			"```md\n" +
			"- delete fake_test.go\n" +
			"```\n"
		got := documentedDeletionAllowlist(body)
		if len(got.ExactPaths) != 0 {
			t.Fatalf("ExactPaths = %v, want empty", got.ExactPaths)
		}
	})

	t.Run("ignores_negated_deletion_instructions", func(t *testing.T) {
		cases := []string{
			"## Scope\n- Do not delete `internal/foo/bar_test.go` under any circumstances\n",
			"## Scope\n- Never remove `internal/foo/bar_test.go`.\n",
			"## Scope\n- Don't delete `internal/foo/bar_test.go`.\n",
			"## Scope\n- We should not delete `internal/foo/bar_test.go`.\n",
			"## Scope\n- Avoid deleting `internal/foo/bar_test.go`.\n",
		}
		for _, body := range cases {
			got := documentedDeletionAllowlist(body)
			if got.ExactPaths["internal/foo/bar_test.go"] {
				t.Fatalf("body %q: ExactPaths = %v, did not want negated deletion path", body, got.ExactPaths)
			}
		}
	})

	t.Run("negation_before_clause_boundary_does_not_block_later_deletion", func(t *testing.T) {
		body := "## Scope\n- Do not edit the live tests, delete `internal/foo/obsolete_test.go`.\n"
		got := documentedDeletionAllowlist(body)
		if !got.ExactPaths["internal/foo/obsolete_test.go"] {
			t.Fatalf("ExactPaths = %v, want explicit deletion after comma boundary", got.ExactPaths)
		}
	})

	t.Run("plan_contract_expected_deletions_add_exact_and_glob_entries", func(t *testing.T) {
		allow := documentedDeletionAllowlistForTrustedSpec(TaskInfo{
			Body: "## Scope\n- delete body_only_test.go\n",
			PlanContract: strings.Replace(validPlanContract("t1"),
				`  "verification": [`,
				`  "expected_deletions": ["internal/foo/legacy_test.go", "testdata/*.golden"],
  "verification": [`, 1),
		})
		if !allow.ExactPaths["body_only_test.go"] {
			t.Fatalf("ExactPaths = %v, want body_only_test.go", allow.ExactPaths)
		}
		if !allow.ExactPaths["internal/foo/legacy_test.go"] {
			t.Fatalf("ExactPaths = %v, want internal/foo/legacy_test.go", allow.ExactPaths)
		}
		if got := allow.ExactPathSource["internal/foo/legacy_test.go"]; !strings.Contains(got, "expected_deletions") {
			t.Fatalf("ExactPathSource = %v, want expected_deletions marker", allow.ExactPathSource)
		}
		if len(allow.Globs) != 1 || allow.Globs[0] != "testdata/*.golden" {
			t.Fatalf("Globs = %v, want testdata/*.golden", allow.Globs)
		}
		if got := allow.GlobSource["testdata/*.golden"]; !strings.Contains(got, "expected_deletions") {
			t.Fatalf("GlobSource = %v, want expected_deletions marker", allow.GlobSource)
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
		{Status: "R100", Path: "new/x_test.go", OldPath: "old/x_test.go"},
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
		return
	}
	if tamper.Type != StepDetectTampering {
		t.Errorf("detect_tampering type = %q, want %q", tamper.Type, StepDetectTampering)
	}

	// verify_commits default (no status condition) must route to codegen_gate,
	// then focused_checks, which hands off to detect_tampering once generated
	// drift is fixed.
	vc := impl.StepByID("verify_commits")
	if vc == nil {
		t.Fatal("verify_commits step missing")
		return
	}
	if got, _ := ResolveTransition(vc.Next, map[string]string{"task.status": "ready-review"}); got != "codegen_gate" {
		t.Errorf("verify_commits default goto = %q, want codegen_gate", got)
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

func TestExecDetectTampering_PlatformGuardedSkipDoesNotFlag(t *testing.T) {
	t.Parallel()
	base := "package agent\n\nimport (\n\t\"testing\"\n)\n\nfunc TestReap(t *testing.T) {\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                   "init\n",
		"internal/agent/reap.go":      "package agent\n\nfunc Reap() {}\n",
		"internal/agent/reap_test.go": base,
	})
	// A skip guarded by runtime.GOOS is a platform guard, not tampering — see
	// https://github.com/Automaat/sybra/issues/2038.
	tampered := "package agent\n\nimport (\n\t\"runtime\"\n\t\"testing\"\n)\n\nfunc TestReap(t *testing.T) {\n\tif runtime.GOOS != \"linux\" {\n\t\tt.Skip(\"linux-only process enumeration test\")\n\t}\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	writeRepoFile(t, wt, "internal/agent/reap_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: skip TestReap on non-linux")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output == "flagged" {
		t.Fatalf("Output = %q, want not flagged (platform-guarded skip)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
	}
}

func TestExecDetectTampering_SkipAfterPlatformGuardFlags(t *testing.T) {
	t.Parallel()
	base := "package agent\n\nimport (\n\t\"testing\"\n)\n\nfunc TestReap(t *testing.T) {\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                   "init\n",
		"internal/agent/reap.go":      "package agent\n\nfunc Reap() {}\n",
		"internal/agent/reap_test.go": base,
	})
	tampered := "package agent\n\nimport (\n\t\"runtime\"\n\t\"testing\"\n)\n\nfunc TestReap(t *testing.T) {\n\tif runtime.GOOS != \"linux\" {\n\t\tsetup()\n\t}\n\tt.Skip(\"flaky\")\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	writeRepoFile(t, wt, "internal/agent/reap_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: add unguarded skip after platform check")

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

func TestExecDetectTampering_UnguardedSkipStillFlags(t *testing.T) {
	t.Parallel()
	base := "package agent\n\nimport (\n\t\"testing\"\n)\n\nfunc TestReap(t *testing.T) {\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                   "init\n",
		"internal/agent/reap.go":      "package agent\n\nfunc Reap() {}\n",
		"internal/agent/reap_test.go": base,
	})
	// An unconditional skip — not guarded by any platform/capability check —
	// must still be flagged as tampering.
	tampered := "package agent\n\nimport (\n\t\"testing\"\n)\n\nfunc TestReap(t *testing.T) {\n\tt.Skip(\"linux-only process enumeration test\")\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	writeRepoFile(t, wt, "internal/agent/reap_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: unconditionally skip TestReap")

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

func TestExecDetectTampering_EstablishedSkipIdiomSurvivesRename(t *testing.T) {
	t.Parallel()
	skipLine := "\tif !hasGit() { t.Skip(\"git not available\") }"
	base := "package foo\n\nimport \"testing\"\n\nfunc hasGit() bool { return true }\n\n" +
		"func TestFoo(t *testing.T) {\n" + skipLine + "\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo.go":      "package foo\n\nfunc Foo() int { return 1 }\n",
		"internal/foo/foo_test.go": base,
	})
	// Rename the file carrying the established idiom and add a new test using
	// the same idiom in the same commit — mirrors a routine `git mv` refactor.
	// The base commit only has the file under the old path, so the base-content
	// lookup must follow the rename rather than looking up the new path (which
	// doesn't exist at the base commit).
	tampered := base + "\nfunc TestBar(t *testing.T) {\n" + skipLine +
		"\n\tif Foo() != 1 {\n\t\tt.Errorf(\"bad\")\n\t}\n}\n"
	if err := os.Remove(filepath.Join(wt, "internal/foo/foo_test.go")); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, wt, "internal/foo/foo_other_test.go", tampered)
	gitRun(t, wt, "add", "-A")
	gitRun(t, wt, "commit", "-m", "test: rename foo_test.go and add bar with established skip guard")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output == "flagged" {
		t.Fatalf("Output = %q, want not flagged (established skip idiom survives rename)", out.Output)
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

func TestExecDetectTampering_DocumentedDeletedTestDoesNotFlag(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                          "init\n",
		"internal/mesh/mesh_helpers_test.go": "package mesh\n\nimport \"testing\"\n\nfunc TestMode(t *testing.T) { t.Errorf(\"x\") }\n",
	})
	gitRun(t, wt, "rm", "internal/mesh/mesh_helpers_test.go")
	gitRun(t, wt, "commit", "-m", "chore: drop documented test")

	engine, tasks := newTamperEngine(t, wt)
	wf := &Execution{Variables: map[string]string{}}
	wf.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "agent-1"})
	captureTamperDeletionAllowlist(wf, "implement", "implementation", TaskInfo{
		ID:   "t1",
		Body: "## Scope\n- delete mesh_helpers_test.go\n",
	})
	tasks.Put(TaskInfo{
		ID:       "t1",
		Status:   "in-progress",
		Workflow: wf,
	})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{
		ID:       "t1",
		Status:   "in-progress",
		Workflow: wf,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
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

func TestExecDetectTampering_DocumentedDeletionUsesPreAgentSnapshot(t *testing.T) {
	t.Parallel()
	base := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo_test.go": base,
	})
	gitRun(t, wt, "rm", "internal/foo/foo_test.go")
	gitRun(t, wt, "commit", "-m", "test: remove foo")

	engine, tasks := newTamperEngine(t, wt)
	wf := &Execution{Variables: map[string]string{}}
	wf.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "agent-1"})
	captureTamperDeletionAllowlist(wf, "implement", "implementation",
		TaskInfo{ID: "t1", Body: "## Scope\n- update implementation only\n"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})

	mutated := TaskInfo{
		ID:       "t1",
		Status:   "in-progress",
		Body:     "## Scope\n- delete `internal/foo/foo_test.go`\n",
		Workflow: wf,
	}
	out, err := engine.execDetectTampering("t1", newTamperStep(), mutated)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged; post-agent body edits must not bless deleted tests", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecDetectTampering_DocumentedDeletionIgnoresLaterAuthorSnapshot(t *testing.T) {
	t.Parallel()
	base := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo_test.go": base,
	})
	gitRun(t, wt, "rm", "internal/foo/foo_test.go")
	gitRun(t, wt, "commit", "-m", "test: remove foo")

	engine, tasks := newTamperEngine(t, wt)
	wf := &Execution{Variables: map[string]string{}}
	captureTamperDeletionAllowlist(wf, "implement", "implementation",
		TaskInfo{ID: "t1", Body: "## Scope\n- update implementation only\n"})
	wf.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "agent-1"})
	captureTamperDeletionAllowlist(wf, "fix_review", "fix-review",
		TaskInfo{ID: "t1", Body: "## Scope\n- delete `internal/foo/foo_test.go`\n"})
	wf.RecordStep(StepRecord{StepID: "fix_review", Status: "completed", AgentID: "agent-2"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{
		ID:       "t1",
		Status:   "in-progress",
		Workflow: wf,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged; later fix-review snapshots must not bless deleted tests", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecDetectTampering_DocumentedDeletionSnapshotDowngrades(t *testing.T) {
	t.Parallel()
	base := "package foo\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                "init\n",
		"internal/foo/foo_test.go": base,
	})
	gitRun(t, wt, "rm", "internal/foo/foo_test.go")
	gitRun(t, wt, "commit", "-m", "test: remove foo")

	engine, tasks := newTamperEngine(t, wt)
	wf := &Execution{Variables: map[string]string{}}
	wf.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "agent-1"})
	captureTamperDeletionAllowlist(wf, "implement", "implementation",
		TaskInfo{ID: "t1", Plan: "## Files\n- delete `internal/foo/foo_test.go`\n"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})

	out, err := engine.execDetectTampering("t1", newTamperStep(),
		TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean; pre-agent snapshot should downgrade documented deletion", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
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

func TestExecDetectTampering_MergedUpstreamSkipNotFlagged(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})

	gitRun(t, wt, "checkout", "-b", "task")
	baseline := strings.TrimSpace(gitOutput(t, wt, "rev-parse", "HEAD"))

	upstreamTest := "package agent\n\nimport (\n\t\"runtime\"\n\t\"testing\"\n)\n\nfunc TestReap(t *testing.T) {\n\tif runtime.GOOS != \"linux\" {\n\t\tt.Skip(\"linux-only process enumeration test\")\n\t}\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	gitRun(t, wt, "checkout", "main")
	writeRepoFile(t, wt, "internal/agent/orphan_sweep_test.go", upstreamTest)
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "test: add orphan sweep")
	gitRun(t, wt, "fetch", "origin")

	gitRun(t, wt, "checkout", "task")
	gitRun(t, wt, "merge", "--no-edit", "origin/main")

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
		t.Fatalf("Output = %q, want clean; a t.Skip merged in from origin/main must not be attributed to the agent", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
	}
}

func TestExecDetectTampering_MergedUpstreamSkipInLocallyEditedFileNotFlagged(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})

	gitRun(t, wt, "checkout", "-b", "task")
	baseline := strings.TrimSpace(gitOutput(t, wt, "rev-parse", "HEAD"))

	upstreamTest := "package agent\n\nimport (\n\t\"runtime\"\n\t\"testing\"\n)\n\nfunc TestReap(t *testing.T) {\n\tif runtime.GOOS != \"linux\" {\n\t\tt.Skip(\"linux-only process enumeration test\")\n\t}\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	gitRun(t, wt, "checkout", "main")
	writeRepoFile(t, wt, "internal/agent/orphan_sweep_test.go", upstreamTest)
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "test: add orphan sweep")
	gitRun(t, wt, "fetch", "origin")

	gitRun(t, wt, "checkout", "task")
	gitRun(t, wt, "merge", "--no-edit", "origin/main")
	writeRepoFile(t, wt, "internal/agent/orphan_sweep_test.go",
		strings.Replace(upstreamTest, "t.Fatal(\"x\")", "t.Fatal(\"updated\")", 1))
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "test: tweak failure text")

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
		t.Fatalf("Output = %q, want clean; merged-in upstream skip must stay ignored even when the file also has local edits", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
	}
}

func TestExecDetectTampering_MergedUpstreamSkipInRenamedFileNotFlagged(t *testing.T) {
	t.Parallel()
	baseTest := "package agent\n\nimport \"testing\"\n\nfunc TestReap(t *testing.T) {\n\tif 1 != 1 {\n\t\tt.Fatal(\"x\")\n\t}\n}\n"
	wt := makeBaseRepo(t, map[string]string{
		"README.md":                           "init\n",
		"internal/agent/orphan_sweep_test.go": baseTest,
	})

	gitRun(t, wt, "checkout", "-b", "task")
	baseline := strings.TrimSpace(gitOutput(t, wt, "rev-parse", "HEAD"))

	upstreamTest := strings.Replace(baseTest, "func TestReap(t *testing.T) {\n", "func TestReap(t *testing.T) {\n\tt.Skip(\"flaky\")\n", 1)
	gitRun(t, wt, "checkout", "main")
	writeRepoFile(t, wt, "internal/agent/orphan_sweep_test.go", upstreamTest)
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "test: add orphan sweep skip")
	gitRun(t, wt, "fetch", "origin")

	gitRun(t, wt, "checkout", "task")
	gitRun(t, wt, "merge", "--no-edit", "origin/main")
	gitRun(t, wt, "mv", "internal/agent/orphan_sweep_test.go", "internal/agent/reap_test.go")
	writeRepoFile(t, wt, "internal/agent/reap_test.go",
		strings.Replace(upstreamTest, "t.Fatal(\"x\")", "t.Fatal(\"updated\")", 1))
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "test: rename and tweak reap test")

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
		t.Fatalf("Output = %q, want clean; upstream skip lookup must follow renamed file old path", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress", ti.Status)
	}
}

// TestExecDetectTampering_OrphanedBaselineFallsBackToOriginBase reproduces the
// force-push scenario from issue #1477: the stored tamper_base baseline stays
// git-resolvable (rev-parse --verify succeeds) but is no longer an ancestor
// of HEAD after the underlying branch was reset/force-pushed. A two-dot diff
// against such an orphaned base would span the whole divergent history
// instead of the agent's actual change; this test asserts the ancestry check
// rejects the stale baseline and falls back to origin/main (three-dot),
// scoping the report to only the real commit.
func TestExecDetectTampering_OrphanedBaselineFallsBackToOriginBase(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{
		"README.md": "init\n",
	})

	// Simulate a prior run's captured baseline: a commit that will later be
	// orphaned by a force-push/reset, but which still resolves via rev-parse.
	writeRepoFile(t, wt, "internal/other/other.go", "package other\n\nfunc Other() int { return 1 }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: unrelated prior work")
	staleBaseline := strings.TrimSpace(gitOutput(t, wt, "rev-parse", "HEAD"))

	// Force-push equivalent: branch is reset back to origin/main, then the
	// agent's real (small) change is committed on top — staleBaseline is now
	// orphaned, resolvable but not an ancestor of the new HEAD.
	gitRun(t, wt, "reset", "--hard", "origin/main")
	writeRepoFile(t, wt, "internal/foo/foo.go", "package foo\n\nfunc Foo() int { return 1 }\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-m", "feat: add foo")

	_, tasks := newTamperEngine(t, wt)
	wf := &Execution{
		Variables: map[string]string{tamperBaselineVar("fix"): staleBaseline},
		StepHistory: []StepRecord{{
			StepID:  "fix",
			Status:  "completed",
			AgentID: "agent-1",
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	base, rangeSpec := resolveTamperRange(context.Background(), wt, TaskInfo{ID: "t1", Workflow: wf}, "t1", nil)
	if base == staleBaseline {
		t.Fatalf("base = %q, want fallback to origin base; orphaned baseline must be rejected", base)
	}
	if rangeSpec == staleBaseline+"..HEAD" {
		t.Fatalf("rangeSpec = %q, want three-dot fallback range, not the stale two-dot baseline range", rangeSpec)
	}
	if !strings.Contains(rangeSpec, "...HEAD") {
		t.Fatalf("rangeSpec = %q, want three-dot fallback range", rangeSpec)
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
		return
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
		return
	}
	fix := rev.StepByID("fix_review")
	if fix == nil {
		t.Fatal("fix_review step missing from simple-task-review")
		return
	}
	// fix_review commits code changes, so verify_checks must re-run before
	// tamper detection to keep verify_checks evidence fresh at the post-fix
	// HEAD (and catch a fix that broke the suite) ahead of require_evidence.
	verify := rev.StepByID("verify_checks")
	if verify == nil {
		t.Fatal("verify_checks step missing from simple-task-review")
		return
	}
	if got, _ := ResolveTransition(fix.Next, map[string]string{"task.status": "ready-review"}); got != "verify_checks" {
		t.Errorf("fix_review goto = %q, want verify_checks", got)
	}
	if got, _ := ResolveTransition(verify.Next, map[string]string{"task.status": "ready-review"}); got != "detect_tampering" {
		t.Errorf("verify_checks goto = %q, want detect_tampering", got)
	}
	if got, _ := ResolveTransition(verify.Next, map[string]string{"task.status": "human-required"}); got != "" {
		t.Errorf("verify_checks human-required goto = %q, want end", got)
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

// TestExecDetectTampering_SelfHostedSkipFixtureDoesNotFlag reproduces issue
// #2323: a task that edits the tamper detector itself and adds a regression
// test in engine_steps_tamper_test.go containing a platform-guarded skip
// fixture (e.g. `if runtime.GOOS != "linux" { t.Skip("linux-only") }`)
// embedded as a Go string constant must not self-deadlock the workflow — the
// detector previously flagged its own fixture text as a live added-skip.
func TestExecDetectTampering_SelfHostedSkipFixtureDoesNotFlag(t *testing.T) {
	t.Parallel()
	base := `package workflow

import "testing"

func TestScanTamperPatchFixture(t *testing.T) {
	_ = 1
}
`
	wt := makeBaseRepo(t, map[string]string{
		"README.md": "init\n",
		"internal/workflow/engine_steps_tamper_test.go": base,
	})
	// Add a regression test covering a platform-guarded skip fixture. The
	// fixture text lives entirely inside a Go string literal (escaped inner
	// quotes never close it), so it is source data, not a real added skip.
	tampered := `package workflow

import "testing"

func TestScanTamperPatchFixture(t *testing.T) {
	_ = 1
}

func TestScanTamperPatchPlatformGuardFixture(t *testing.T) {
	patch := "@@ @@\n func TestReap(t *testing.T) {\n+\tif runtime.GOOS != \"linux\" { t.Skip(\"linux-only\") }\n"
	_ = patch
}
`
	writeRepoFile(t, wt, "internal/workflow/engine_steps_tamper_test.go", tampered)
	gitRun(t, wt, "commit", "-am", "test: cover platform-guarded skip fixture")

	engine, tasks := newTamperEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execDetectTampering("t1", newTamperStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output == "flagged" {
		t.Fatalf("Output = %q, want not flagged (self-hosted fixture, not a real skip)", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress — workflow must not self-deadlock", ti.Status)
	}
}
