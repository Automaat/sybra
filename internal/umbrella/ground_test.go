package umbrella

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestExtractPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			"backtick path",
			"Edit `internal/foo/bar.go` to fix this.",
			[]string{"internal/foo/bar.go"},
		},
		{
			"plain text path",
			"Look at internal/foo/bar.go for the bug.",
			[]string{"internal/foo/bar.go"},
		},
		{
			"trailing punctuation stripped",
			"See `internal/foo/bar.go,` and (internal/baz/qux.go).",
			[]string{"internal/foo/bar.go", "internal/baz/qux.go"},
		},
		{
			"skips URLs",
			"See https://github.com/o/r/blob/main/internal/foo.go for context.",
			nil,
		},
		{
			"skips owner/repo#n refs",
			"Depends on `Automaat/sybra#123` finishing first.",
			nil,
		},
		{
			"skips bare #n refs",
			"See #123 for the discussion.",
			nil,
		},
		{
			"dedups case-insensitively",
			"Touches `internal/Foo/bar.go` and internal/foo/bar.go again.",
			[]string{"internal/Foo/bar.go"},
		},
		{
			"files with spaces via backtick",
			"Edit `internal/my file/bar.go` please.",
			[]string{"internal/my file/bar.go"},
		},
		{
			"bare word without slash is not a path",
			"Update the README and CHANGELOG.",
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractPaths(tt.body)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractPaths(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestConfirm(t *testing.T) {
	t.Parallel()
	fs := newFileSet([]string{
		"internal/foo/bar.go",
		"internal/foobar/baz.go",
	})

	tests := []struct {
		name     string
		token    string
		wantOK   bool
		wantPath string
	}{
		{"tracked file matches", "internal/foo/bar.go", true, "internal/foo/bar.go"},
		{"real ancestor dir matches", "internal/foo", true, "internal/foo"},
		{"sibling file does not collide", "internal/foo/other.go", false, ""},
		{"unrelated path does not match", "internal/bar", false, ""},
		{"casing is normalized", "INTERNAL/FOO/BAR.GO", true, "internal/foo/bar.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := fs.confirm(tt.token)
			if ok != tt.wantOK {
				t.Fatalf("confirm(%q) ok = %v, want %v", tt.token, ok, tt.wantOK)
			}
			if ok && got != tt.wantPath {
				t.Errorf("confirm(%q) = %q, want %q", tt.token, got, tt.wantPath)
			}
		})
	}

	// internal/foo must never be confirmed as an ancestor of internal/foobar
	// (segment-wise comparison, not string-prefix).
	if _, ok := fs.confirm("internal/foobarbaz"); ok {
		t.Error("internal/foobarbaz must not confirm against internal/foo or internal/foobar")
	}
}

func TestGroundTouches(t *testing.T) {
	t.Parallel()
	fs := newFileSet([]string{"internal/foo/bar.go", "internal/baz/qux.go"})
	body := "Edit `internal/foo/bar.go` and mention https://example.com/internal/nope.go, " +
		"also touches internal/foo/bar.go again and internal/baz."
	got := groundTouches(fs, body)
	want := []string{"internal/foo/bar.go", "internal/baz"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groundTouches = %v, want %v", got, want)
	}
}

func TestPlanGroundGate(t *testing.T) {
	t.Parallel()
	lister := func(_ context.Context, _ string) ([]string, error) {
		return []string{"internal/foo/bar.go"}, nil
	}

	newPlanSubs := func(n int) ([]SubIssue, *Plan) {
		s := make([]SubIssue, n)
		p := &Plan{}
		for i := range n {
			ref := "o/r#" + string(rune('1'+i))
			s[i] = SubIssue{Ref: ref, Body: "touches `internal/foo/bar.go`"}
			p.Children = append(p.Children, PlannedChild{Ref: ref})
		}
		return s, p
	}

	tests := []struct {
		name       string
		minSubs    int
		numSubs    int
		wantGround bool
	}{
		{"zero threshold always grounds", 0, 2, true},
		{"negative threshold always grounds", -1, 2, true},
		{"below threshold skips", 3, 2, false},
		{"at threshold grounds", 2, 2, true},
		{"above threshold grounds", 1, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subs, plan := newPlanSubs(tt.numSubs)
			report := plan.ground(context.Background(), lister, subs, tt.minSubs)
			grounded := len(plan.Children[0].Touches) > 0
			if grounded != tt.wantGround {
				t.Fatalf("grounded = %v, want %v (touches=%v)", grounded, tt.wantGround, plan.Children[0].Touches)
			}
			if tt.wantGround && len(report.groundedRepos) == 0 {
				t.Error("expected groundedRepos to be non-empty when grounding ran")
			}
			if !tt.wantGround && len(report.groundedRepos) != 0 {
				t.Errorf("groundedRepos = %v, want empty when gated off", report.groundedRepos)
			}
		})
	}
}

func TestPlanGroundFailOpen(t *testing.T) {
	t.Parallel()

	t.Run("lister error skips and leaves touches unchanged", func(t *testing.T) {
		t.Parallel()
		subs := []SubIssue{{Ref: "o/r#1", Body: "touches `internal/foo/bar.go`"}}
		plan := &Plan{Children: []PlannedChild{{Ref: "o/r#1", Touches: []string{"existing/path"}}}}
		lister := func(_ context.Context, _ string) ([]string, error) {
			return nil, errors.New("boom")
		}
		report := plan.ground(context.Background(), lister, subs, 0)
		if !reflect.DeepEqual(plan.Children[0].Touches, []string{"existing/path"}) {
			t.Errorf("Touches = %v, want unchanged [existing/path]", plan.Children[0].Touches)
		}
		if len(report.skipped) != 1 || report.skipped[0] != "o/r" {
			t.Errorf("skipped = %v, want [o/r]", report.skipped)
		}
		if len(report.groundedRepos) != 0 {
			t.Errorf("groundedRepos = %v, want empty", report.groundedRepos)
		}
	})

	t.Run("unresolvable ref skips and leaves touches unchanged", func(t *testing.T) {
		t.Parallel()
		subs := []SubIssue{{Ref: "not-a-valid-ref", Body: "touches `internal/foo/bar.go`"}}
		plan := &Plan{Children: []PlannedChild{{Ref: "not-a-valid-ref"}}}
		lister := func(_ context.Context, _ string) ([]string, error) {
			return []string{"internal/foo/bar.go"}, nil
		}
		report := plan.ground(context.Background(), lister, subs, 0)
		if len(plan.Children[0].Touches) != 0 {
			t.Errorf("Touches = %v, want empty", plan.Children[0].Touches)
		}
		if len(report.skipped) != 1 || report.skipped[0] != "not-a-valid-ref" {
			t.Errorf("skipped = %v, want [not-a-valid-ref]", report.skipped)
		}
	})

	t.Run("nil lister is a no-op", func(t *testing.T) {
		t.Parallel()
		subs := []SubIssue{{Ref: "o/r#1", Body: "touches `internal/foo/bar.go`"}}
		plan := &Plan{Children: []PlannedChild{{Ref: "o/r#1"}}}
		report := plan.ground(context.Background(), nil, subs, 0)
		if len(plan.Children[0].Touches) != 0 {
			t.Errorf("Touches = %v, want empty (grounding skipped)", plan.Children[0].Touches)
		}
		if len(report.skipped) != 0 || len(report.groundedRepos) != 0 {
			t.Errorf("report = %+v, want empty", report)
		}
	})

	t.Run("lister called once per repo across multiple children", func(t *testing.T) {
		t.Parallel()
		calls := 0
		lister := func(_ context.Context, _ string) ([]string, error) {
			calls++
			return []string{"internal/foo/bar.go"}, nil
		}
		subs := []SubIssue{
			{Ref: "o/r#1", Body: "touches `internal/foo/bar.go`"},
			{Ref: "o/r#2", Body: "touches `internal/foo/bar.go`"},
		}
		plan := &Plan{Children: []PlannedChild{{Ref: "o/r#1"}, {Ref: "o/r#2"}}}
		plan.ground(context.Background(), lister, subs, 0)
		if calls != 1 {
			t.Errorf("lister called %d times, want 1 (memoized per repo)", calls)
		}
	})
}

func TestUnionTouches(t *testing.T) {
	t.Parallel()
	got := unionTouches([]string{"internal/foo", "internal/bar"}, []string{"INTERNAL/FOO", "internal/baz"})
	want := []string{"internal/foo", "internal/bar", "internal/baz"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unionTouches = %v, want %v", got, want)
	}
}
