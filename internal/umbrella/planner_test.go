package umbrella

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func subs(refs ...string) []SubIssue {
	out := make([]SubIssue, len(refs))
	for i, r := range refs {
		out[i] = SubIssue{Ref: r, Title: "t" + r}
	}
	return out
}

func TestBuildPrompt_SerializesSameFileSubIssues(t *testing.T) {
	t.Parallel()
	prompt := BuildPrompt("o/r#100", "body", subs("o/r#1", "o/r#2"))
	// The planner must be told to report change-surface metadata (touches/
	// produces/requires) instead of reading a dependency graph, and that
	// overlapping touches get serialized automatically.
	for _, want := range []string{
		"SAME files", "merge one at a time", "false overlap",
		"touches", "produces", "requires", "derived automatically",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing metadata guidance %q:\n%s", want, prompt)
		}
	}
}

func TestFirstJSONObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
		ok             bool
	}{
		{"plain", `{"a":1}`, `{"a":1}`, true},
		{"prose around", "sure:\n{\"a\":1}\ndone", `{"a":1}`, true},
		{"code fence", "```json\n{\"a\":1}\n```", `{"a":1}`, true},
		{"nested", `{"a":{"b":2}}`, `{"a":{"b":2}}`, true},
		{"brace in string", `{"a":"}"}`, `{"a":"}"}`, true},
		{"escaped quote in string", `{"a":"x\"}y"}`, `{"a":"x\"}y"}`, true},
		{"none", "no json here", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := firstJSONObject(c.in)
			if ok != c.ok || got != c.want {
				t.Fatalf("firstJSONObject(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestParsePlan(t *testing.T) {
	t.Parallel()
	want := `{"children":[{"issue":"o/r#1","dependsOn":[]}],"maxParallel":3}`
	cases := []struct {
		name, raw string
	}{
		{"plain json", want},
		{"claude envelope", `{"type":"result","result":"` + strings.ReplaceAll(want, `"`, `\"`) + `"}`},
		{"fenced in prose", "Here is the plan:\n```json\n" + want + "\n```"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, err := ParsePlan(c.raw)
			if err != nil {
				t.Fatalf("ParsePlan: %v", err)
			}
			if len(p.Children) != 1 || p.Children[0].Ref != "o/r#1" || p.MaxParallel != 3 {
				t.Fatalf("unexpected plan: %+v", p)
			}
		})
	}

	if _, err := ParsePlan("no json at all"); err == nil {
		t.Error("expected error on output with no JSON object")
	}
}

func TestPlanResolve(t *testing.T) {
	t.Parallel()
	idx := buildRefIndex(subs("o/r#1", "o/r#2"))

	t.Run("shorthand and self-dep", func(t *testing.T) {
		t.Parallel()
		// #2 depends on bare "#1" (shorthand) and on itself; resolve must
		// canonicalize the shorthand and drop the self-dep.
		p := Plan{Children: []PlannedChild{
			{Ref: "o/r#1"},
			{Ref: "https://github.com/o/r/issues/2", DependsOn: []string{"#1", "o/r#2"}},
		}}
		if err := p.resolve(idx); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if p.Children[1].Ref != "o/r#2" {
			t.Errorf("child ref not canonicalized: %q", p.Children[1].Ref)
		}
		if len(p.Children[1].DependsOn) != 1 || p.Children[1].DependsOn[0] != "o/r#1" {
			t.Errorf("deps = %v, want [o/r#1] (shorthand resolved, self-dep dropped)", p.Children[1].DependsOn)
		}
	})

	t.Run("unknown child ref errors", func(t *testing.T) {
		t.Parallel()
		p := Plan{Children: []PlannedChild{{Ref: "o/r#404"}}}
		if err := p.resolve(idx); err == nil {
			t.Error("expected error for hallucinated child ref")
		}
	})

	t.Run("unresolved dependency errors loudly", func(t *testing.T) {
		t.Parallel()
		p := Plan{Children: []PlannedChild{{Ref: "o/r#1", DependsOn: []string{"o/r#999"}}}}
		if err := p.resolve(idx); err == nil {
			t.Error("expected error for unresolved dependency (must not be silently dropped)")
		}
	})
}

func TestPlanValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		plan    Plan
		subs    []SubIssue
		wantErr string
	}{
		{
			name: "valid acyclic",
			plan: Plan{Children: []PlannedChild{
				{Ref: "o/r#1"},
				{Ref: "o/r#2", DependsOn: []string{"o/r#1"}},
			}},
			subs: subs("o/r#1", "o/r#2"),
		},
		{
			name:    "incomplete coverage",
			plan:    Plan{Children: []PlannedChild{{Ref: "o/r#1"}}},
			subs:    subs("o/r#1", "o/r#2"),
			wantErr: "covered 1 of 2",
		},
		{
			name: "duplicate child",
			plan: Plan{Children: []PlannedChild{
				{Ref: "o/r#1"},
				{Ref: "o/r#1"},
			}},
			subs:    subs("o/r#1", "o/r#2"),
			wantErr: "more than once",
		},
		{
			name: "cycle",
			plan: Plan{Children: []PlannedChild{
				{Ref: "o/r#1", DependsOn: []string{"o/r#2"}},
				{Ref: "o/r#2", DependsOn: []string{"o/r#1"}},
			}},
			subs:    subs("o/r#1", "o/r#2"),
			wantErr: "cycle",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.plan.validate(tt.subs)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeriveEdges(t *testing.T) {
	t.Parallel()

	depsOf := func(plan Plan, ref string) []string {
		for _, c := range plan.Children {
			if c.Ref == ref {
				return c.DependsOn
			}
		}
		t.Fatalf("no child %s in plan", ref)
		return nil
	}

	tests := []struct {
		name     string
		subs     []SubIssue
		children []PlannedChild
		want     map[string][]string // ref -> expected DependsOn (order-sensitive)
	}{
		{
			name: "produce/require match",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1", Produces: []string{"Foo.Run"}},
				{Ref: "o/r#2", Requires: []string{"foo.run"}}, // case-insensitive match
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}},
		},
		{
			name: "multiple producers of the same symbol => depend on all",
			subs: subs("o/r#1", "o/r#2", "o/r#3"),
			children: []PlannedChild{
				{Ref: "o/r#1", Produces: []string{"Sym"}},
				{Ref: "o/r#2", Produces: []string{"Sym"}},
				{Ref: "o/r#3", Requires: []string{"Sym"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": nil, "o/r#3": {"o/r#1", "o/r#2"}},
		},
		{
			name: "touch overlap including ancestor directory",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1", Touches: []string{"internal/foo"}},
				{Ref: "o/r#2", Touches: []string{"internal/foo/x.go"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}},
		},
		{
			name: "sibling directories do not overlap by string prefix",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1", Touches: []string{"internal/foo"}},
				{Ref: "o/r#2", Touches: []string{"internal/foobar"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": nil},
		},
		{
			name: "explicit dependsOn is unioned with derived edges",
			subs: subs("o/r#1", "o/r#2", "o/r#3"),
			children: []PlannedChild{
				{Ref: "o/r#1", Produces: []string{"Sym"}},
				{Ref: "o/r#2"},
				{Ref: "o/r#3", Requires: []string{"Sym"}, DependsOn: []string{"o/r#2"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": nil, "o/r#3": {"o/r#2", "o/r#1"}},
		},
		{
			name: "legacy dependsOn-only input is a no-op",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1"},
				{Ref: "o/r#2", DependsOn: []string{"o/r#1"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}},
		},
		{
			name: "self-edge is dropped",
			subs: subs("o/r#1"),
			children: []PlannedChild{
				{Ref: "o/r#1", Produces: []string{"Sym"}, Requires: []string{"Sym"}},
			},
			want: map[string][]string{"o/r#1": nil},
		},
		{
			name: "closed-producer edge is derived here (dropped later by ChildSpecs)",
			subs: []SubIssue{
				{Ref: "o/r#1", Closed: true},
				{Ref: "o/r#2"},
			},
			children: []PlannedChild{
				{Ref: "o/r#1", Produces: []string{"Sym"}},
				{Ref: "o/r#2", Requires: []string{"Sym"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan := Plan{Children: append([]PlannedChild(nil), tt.children...)}
			plan.deriveEdges(tt.subs)
			for ref, want := range tt.want {
				got := depsOf(plan, ref)
				if len(got) != len(want) {
					t.Fatalf("%s: DependsOn = %v, want %v", ref, got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("%s: DependsOn = %v, want %v", ref, got, want)
					}
				}
			}
		})
	}

	t.Run("determinism independent of plan.Children slice order", func(t *testing.T) {
		t.Parallel()
		s := subs("o/r#1", "o/r#2", "o/r#3")
		forward := []PlannedChild{
			{Ref: "o/r#1", Produces: []string{"Sym"}, Touches: []string{"internal/foo"}},
			{Ref: "o/r#2", Produces: []string{"Sym"}, Touches: []string{"internal/foo/x.go"}},
			{Ref: "o/r#3", Requires: []string{"Sym"}},
		}
		reversed := []PlannedChild{forward[2], forward[1], forward[0]}

		p1 := Plan{Children: append([]PlannedChild(nil), forward...)}
		p1.deriveEdges(s)
		p2 := Plan{Children: append([]PlannedChild(nil), reversed...)}
		p2.deriveEdges(s)

		assertEqual := func(t *testing.T, got, want []string, msg string) {
			t.Helper()
			if len(got) != len(want) {
				t.Fatalf("%s: forward=%v reversed=%v", msg, got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s: forward=%v reversed=%v", msg, got, want)
				}
			}
		}
		assertEqual(t, depsOf(p1, "o/r#3"), depsOf(p2, "o/r#3"), "non-deterministic")
		assertEqual(t, depsOf(p1, "o/r#2"), depsOf(p2, "o/r#2"), "non-deterministic touch overlap")
	})
}

func TestGenerate(t *testing.T) {
	t.Parallel()
	good := `{"children":[{"issue":"o/r#1","produces":["Foo.Run"]},{"issue":"o/r#2","requires":["Foo.Run"]}],"maxParallel":2}`

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		run := func(_ context.Context, prompt string) (string, error) {
			// Prompt must carry the sub-issue refs so the model can use them.
			if !strings.Contains(prompt, "o/r#1") || !strings.Contains(prompt, "o/r#2") {
				t.Errorf("prompt missing sub-issue refs:\n%s", prompt)
			}
			return good, nil
		}
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2"))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if plan.MaxParallel != 2 || len(plan.Children) != 2 {
			t.Fatalf("unexpected plan: %+v", plan)
		}
		var child2 PlannedChild
		for _, c := range plan.Children {
			if c.Ref == "o/r#2" {
				child2 = c
			}
		}
		if len(child2.DependsOn) != 1 || child2.DependsOn[0] != "o/r#1" {
			t.Fatalf("requires->produces edge not derived: %+v", child2)
		}
	})

	t.Run("shorthand deps resolve end to end", func(t *testing.T) {
		t.Parallel()
		// Model emits the dependency in bare "#1" shorthand — the natural form
		// given the prompt's "← #N" markers. Must resolve, not silently drop.
		shorthand := `{"children":[{"issue":"o/r#1","dependsOn":[]},{"issue":"o/r#2","dependsOn":["#1"]}],"maxParallel":2}`
		run := func(_ context.Context, _ string) (string, error) { return shorthand, nil }
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2"))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		var child2 PlannedChild
		for _, c := range plan.Children {
			if c.Ref == "o/r#2" {
				child2 = c
			}
		}
		if len(child2.DependsOn) != 1 || child2.DependsOn[0] != "o/r#1" {
			t.Fatalf("shorthand dep not resolved: %+v", child2)
		}
	})

	t.Run("runner error", func(t *testing.T) {
		t.Parallel()
		run := func(_ context.Context, _ string) (string, error) { return "", errors.New("boom") }
		if _, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1")); err == nil {
			t.Error("expected runner error to propagate")
		}
	})

	t.Run("no sub-issues", func(t *testing.T) {
		t.Parallel()
		run := func(_ context.Context, _ string) (string, error) { return good, nil }
		if _, err := Generate(context.Background(), run, "o/r#100", "body", nil); err == nil {
			t.Error("expected error when umbrella has no sub-issues")
		}
	})

	t.Run("cycle rejected", func(t *testing.T) {
		t.Parallel()
		cyclic := `{"children":[{"issue":"o/r#1","dependsOn":["o/r#2"]},{"issue":"o/r#2","dependsOn":["o/r#1"]}],"maxParallel":2}`
		run := func(_ context.Context, _ string) (string, error) { return cyclic, nil }
		if _, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2")); err == nil {
			t.Error("expected cycle to be rejected")
		}
	})

	t.Run("cycle derived from mutual requires/produces is rejected", func(t *testing.T) {
		t.Parallel()
		// No explicit dependsOn — the cycle only exists once deriveEdges turns
		// each side's "requires" into an edge on the other's "produces".
		cyclic := `{"children":[` +
			`{"issue":"o/r#1","produces":["A"],"requires":["B"]},` +
			`{"issue":"o/r#2","produces":["B"],"requires":["A"]}` +
			`],"maxParallel":2}`
		run := func(_ context.Context, _ string) (string, error) { return cyclic, nil }
		if _, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2")); err == nil {
			t.Error("expected derived cycle to be rejected")
		}
	})

	flat := `{"children":[{"issue":"o/r#1"},{"issue":"o/r#2"},{"issue":"o/r#3"}],"maxParallel":2}`
	edged := `{"children":[{"issue":"o/r#1"},{"issue":"o/r#2","dependsOn":["o/r#1"]},{"issue":"o/r#3"}],"maxParallel":2}`

	t.Run("re-ask fires and returns edged plan", func(t *testing.T) {
		t.Parallel()
		calls := 0
		run := func(_ context.Context, prompt string) (string, error) {
			calls++
			if strings.Contains(prompt, criticSuffix) {
				return edged, nil
			}
			return flat, nil
		}
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2", "o/r#3"))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if calls != 2 {
			t.Fatalf("expected exactly one re-ask (2 total calls), got %d", calls)
		}
		var child2 PlannedChild
		for _, c := range plan.Children {
			if c.Ref == "o/r#2" {
				child2 = c
			}
		}
		if len(child2.DependsOn) != 1 || child2.DependsOn[0] != "o/r#1" {
			t.Fatalf("expected edged re-ask plan to be returned: %+v", plan)
		}
	})

	t.Run("re-ask still flat is accepted", func(t *testing.T) {
		t.Parallel()
		calls := 0
		run := func(_ context.Context, _ string) (string, error) {
			calls++
			return flat, nil
		}
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2", "o/r#3"))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if calls != 2 {
			t.Fatalf("expected exactly one re-ask (2 total calls), got %d", calls)
		}
		for _, c := range plan.Children {
			if len(c.DependsOn) != 0 {
				t.Fatalf("expected still-flat plan to be accepted as-is: %+v", plan)
			}
		}
	})

	t.Run("re-ask parse error falls back to original plan", func(t *testing.T) {
		t.Parallel()
		calls := 0
		run := func(_ context.Context, prompt string) (string, error) {
			calls++
			if strings.Contains(prompt, criticSuffix) {
				return "not json", nil
			}
			return flat, nil
		}
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2", "o/r#3"))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if calls != 1+plannerAttempts {
			t.Fatalf("expected 1 initial call + %d re-ask attempts, got %d", plannerAttempts, calls)
		}
		if len(plan.Children) != 3 {
			t.Fatalf("expected fallback to original flat plan: %+v", plan)
		}
	})

	t.Run("re-ask context deadline falls back to original plan", func(t *testing.T) {
		t.Parallel()
		run := func(_ context.Context, prompt string) (string, error) {
			if strings.Contains(prompt, criticSuffix) {
				return "", context.DeadlineExceeded
			}
			return flat, nil
		}
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2", "o/r#3"))
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(plan.Children) != 3 {
			t.Fatalf("expected fallback to original flat plan: %+v", plan)
		}
	})

	t.Run("no re-ask when fewer than three non-done children", func(t *testing.T) {
		t.Parallel()
		calls := 0
		run := func(_ context.Context, _ string) (string, error) {
			calls++
			return good, nil
		}
		if _, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2")); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if calls != 1 {
			t.Fatalf("expected no re-ask below the 3-child floor, got %d calls", calls)
		}
	})
}

func TestFlatPlanSuspicious(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		plan Plan
		subs []SubIssue
		want bool
	}{
		{
			name: "below threshold not suspicious",
			plan: Plan{Children: []PlannedChild{{Ref: "o/r#1"}, {Ref: "o/r#2"}}},
			subs: subs("o/r#1", "o/r#2"),
			want: false,
		},
		{
			name: "flat with three non-done children is suspicious",
			plan: Plan{Children: []PlannedChild{{Ref: "o/r#1"}, {Ref: "o/r#2"}, {Ref: "o/r#3"}}},
			subs: subs("o/r#1", "o/r#2", "o/r#3"),
			want: true,
		},
		{
			name: "any derived edge is not suspicious",
			plan: Plan{Children: []PlannedChild{
				{Ref: "o/r#1"},
				{Ref: "o/r#2", DependsOn: []string{"o/r#1"}},
				{Ref: "o/r#3"},
			}},
			subs: subs("o/r#1", "o/r#2", "o/r#3"),
			want: false,
		},
		{
			name: "closed subs do not count toward the threshold",
			plan: Plan{Children: []PlannedChild{{Ref: "o/r#1"}, {Ref: "o/r#2"}, {Ref: "o/r#3"}}},
			subs: []SubIssue{
				{Ref: "o/r#1", Closed: true},
				{Ref: "o/r#2"},
				{Ref: "o/r#3"},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := flatPlanSuspicious(tt.plan, tt.subs); got != tt.want {
				t.Errorf("flatPlanSuspicious() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFallbackPlannerRunnerPassesConfiguredClaudeModel(t *testing.T) {
	dir := t.TempDir()
	writePlannerTestExe(t, filepath.Join(dir, "claude"), `#!/bin/bash
if [[ "$*" != *"--model opus"* ]]; then
  echo "missing configured model flag: $*" >&2
  exit 7
fi
printf '%s\n' '{"result":"{\"children\":[{\"issue\":\"o/r#1\",\"dependsOn\":[]}],\"maxParallel\":1}"}'
`)
	t.Setenv("PATH", dir)

	out, err := FallbackPlannerRunner("opus")(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if !strings.Contains(out, `"maxParallel":1`) {
		t.Fatalf("planner output = %q, want marshaled plan", out)
	}
}

func writePlannerTestExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
