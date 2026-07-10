package umbrella

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
		"almost always share code", "dependency exists between any two",
		"safe, zero-cost default", "parallelJustification",
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

	t.Run("unknown parallelJustification key is dropped, not an error", func(t *testing.T) {
		t.Parallel()
		// Unlike DependsOn, a hallucinated justification key must fail closed
		// (drop, keep the pair serial) rather than abort the whole plan.
		p := Plan{Children: []PlannedChild{
			{Ref: "o/r#1"},
			{Ref: "o/r#2", ParallelJustification: map[string]string{"o/r#404": "disjoint"}},
		}}
		if err := p.resolve(idx); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(p.Children[1].ParallelJustification) != 0 {
			t.Errorf("ParallelJustification = %v, want dropped", p.Children[1].ParallelJustification)
		}
	})

	t.Run("self-key parallelJustification is dropped", func(t *testing.T) {
		t.Parallel()
		p := Plan{Children: []PlannedChild{
			{Ref: "o/r#1", ParallelJustification: map[string]string{"o/r#1": "self, nonsensical"}},
		}}
		if err := p.resolve(idx); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(p.Children[0].ParallelJustification) != 0 {
			t.Errorf("ParallelJustification = %v, want self-key dropped", p.Children[0].ParallelJustification)
		}
	})

	t.Run("whitespace-only parallelJustification value is dropped", func(t *testing.T) {
		t.Parallel()
		p := Plan{Children: []PlannedChild{
			{Ref: "o/r#1"},
			{Ref: "o/r#2", ParallelJustification: map[string]string{"o/r#1": "   "}},
		}}
		if err := p.resolve(idx); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(p.Children[1].ParallelJustification) != 0 {
			t.Errorf("ParallelJustification = %v, want blank value dropped", p.Children[1].ParallelJustification)
		}
	})

	t.Run("shorthand parallelJustification key is canonicalized", func(t *testing.T) {
		t.Parallel()
		p := Plan{Children: []PlannedChild{
			{Ref: "o/r#1"},
			{Ref: "o/r#2", ParallelJustification: map[string]string{"#1": "disjoint dirs"}},
		}}
		if err := p.resolve(idx); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got := p.Children[1].ParallelJustification["o/r#1"]; got != "disjoint dirs" {
			t.Errorf("ParallelJustification = %v, want canonical key o/r#1", p.Children[1].ParallelJustification)
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
			// o/r#2 used to have no deps; the serial-default layer now adds its
			// only earlier canonical sibling (#1), since no justification exists.
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}, "o/r#3": {"o/r#1", "o/r#2"}},
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
			name: "sibling directories do not overlap and are mutually justified parallel",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1", Touches: []string{"internal/foo"}, ParallelJustification: map[string]string{"o/r#2": "disjoint packages"}},
				{Ref: "o/r#2", Touches: []string{"internal/foobar"}, ParallelJustification: map[string]string{"o/r#1": "disjoint packages"}},
			},
			// Proves both pathsOverlap (no touch-derived edge) and the
			// justification exemption (no serial-default edge either).
			want: map[string][]string{"o/r#1": nil, "o/r#2": nil},
		},
		{
			name: "serial-default fires with no justification even when directories look disjoint",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1", Touches: []string{"internal/foo"}},
				{Ref: "o/r#2", Touches: []string{"internal/foobar"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}},
		},
		{
			name: "explicit dependsOn is unioned with derived edges",
			subs: subs("o/r#1", "o/r#2", "o/r#3"),
			children: []PlannedChild{
				{Ref: "o/r#1", Produces: []string{"Sym"}},
				{Ref: "o/r#2"},
				{Ref: "o/r#3", Requires: []string{"Sym"}, DependsOn: []string{"o/r#2"}},
			},
			// o/r#2 used to have no deps; serial-default now adds #1.
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}, "o/r#3": {"o/r#2", "o/r#1"}},
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
		{
			name: "no metadata at all still serializes end to end",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1"},
				{Ref: "o/r#2"},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}},
		},
		{
			name: "one-sided justification exempts the pair (OR semantics)",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1"},
				{Ref: "o/r#2", ParallelJustification: map[string]string{"o/r#1": "disjoint surfaces"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": nil},
		},
		{
			name: "mutual (both-sided) justification also exempts the pair",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1", ParallelJustification: map[string]string{"o/r#2": "disjoint surfaces"}},
				{Ref: "o/r#2", ParallelJustification: map[string]string{"o/r#1": "disjoint surfaces"}},
			},
			// Guards against an accidental OR->AND regression: both sides
			// carrying a justification must not somehow re-add the edge.
			want: map[string][]string{"o/r#1": nil, "o/r#2": nil},
		},
		{
			name: "justified pair still re-serializes on overlapping touches",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				{Ref: "o/r#1", Touches: []string{"internal/foo"}, ParallelJustification: map[string]string{"o/r#2": "thought disjoint"}},
				{Ref: "o/r#2", Touches: []string{"internal/foo/x.go"}, ParallelJustification: map[string]string{"o/r#1": "thought disjoint"}},
			},
			want: map[string][]string{"o/r#1": nil, "o/r#2": {"o/r#1"}},
		},
		{
			name: "producer listed after its consumer avoids a direct 2-cycle",
			subs: subs("o/r#1", "o/r#2"),
			children: []PlannedChild{
				// o/r#1 is canonically earlier but requires a symbol only
				// o/r#2 (canonically later) produces, forcing a forward
				// requires edge #1 -> #2. Serial-default must not also add
				// #2 -> #1, which would be a direct 2-cycle.
				{Ref: "o/r#1", Requires: []string{"Sym"}},
				{Ref: "o/r#2", Produces: []string{"Sym"}},
			},
			want: map[string][]string{"o/r#1": {"o/r#2"}, "o/r#2": nil},
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
		s := subs("o/r#1", "o/r#2", "o/r#3", "o/r#4")
		forward := []PlannedChild{
			{Ref: "o/r#1", Produces: []string{"Sym"}, Touches: []string{"internal/foo"}},
			{Ref: "o/r#2", Produces: []string{"Sym"}, Touches: []string{"internal/foo/x.go"}},
			{Ref: "o/r#3", Requires: []string{"Sym"}},
			{Ref: "o/r#4"}, // no metadata at all: purely a serial-default case
		}
		reversed := []PlannedChild{forward[3], forward[2], forward[1], forward[0]}

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
		assertEqual(t, depsOf(p1, "o/r#1"), depsOf(p2, "o/r#1"), "non-deterministic")
		assertEqual(t, depsOf(p1, "o/r#2"), depsOf(p2, "o/r#2"), "non-deterministic touch overlap")
		assertEqual(t, depsOf(p1, "o/r#3"), depsOf(p2, "o/r#3"), "non-deterministic")
		assertEqual(t, depsOf(p1, "o/r#4"), depsOf(p2, "o/r#4"), "non-deterministic serial-default")
		// o/r#4 carries no metadata at all: the serial-default layer must still
		// serialize it after every earlier canonical sibling, regardless of
		// where the model happened to place it in plan.Children.
		if got := depsOf(p1, "o/r#4"); len(got) != 3 {
			t.Fatalf("o/r#4 DependsOn = %v, want all 3 earlier canonical siblings", got)
		}
	})

	t.Run("transitive requires-before-produces stays acyclic through serial-default", func(t *testing.T) {
		t.Parallel()
		// o/r#1 is canonically earlier but requires a symbol only the later
		// o/r#2 produces (forward requires edge). o/r#3 carries no metadata.
		// The serial-default layer adds edges from o/r#3 to both earlier
		// siblings, and must not add o/r#2 -> o/r#1 (would be a direct
		// 2-cycle against the forward requires edge). The resulting DAG must
		// still validate as acyclic.
		s := subs("o/r#1", "o/r#2", "o/r#3")
		plan := Plan{Children: []PlannedChild{
			{Ref: "o/r#1", Requires: []string{"Sym"}},
			{Ref: "o/r#2", Produces: []string{"Sym"}},
			{Ref: "o/r#3"},
		}}
		plan.deriveEdges(s)
		if err := plan.validate(s); err != nil {
			t.Fatalf("validate: %v (deps: %+v)", err, plan.Children)
		}
		if got := depsOf(plan, "o/r#1"); len(got) != 1 || got[0] != "o/r#2" {
			t.Fatalf("o/r#1 DependsOn = %v, want [o/r#2]", got)
		}
		if got := depsOf(plan, "o/r#2"); len(got) != 0 {
			t.Fatalf("o/r#2 DependsOn = %v, want none (2-cycle guard)", got)
		}
	})

	t.Run("indirect cycle through an intermediate sibling stays acyclic", func(t *testing.T) {
		t.Parallel()
		// o/r#1 is canonically first but requires a symbol only the
		// canonically-last o/r#3 produces (forward requires edge #1 -> #3).
		// o/r#2 sits between them with no metadata. A direct-only 2-cycle
		// guard misses this: pass 2 adds #2 -> #1 (no direct edge #1 -> #2),
		// then reaches #3 vs #2 and — checking only the direct edge — adds
		// #3 -> #2, closing #1 -> #3 -> #2 -> #1. The guard must walk
		// transitively so the reverse edge (#3 -> #2) is skipped instead.
		s := subs("o/r#1", "o/r#2", "o/r#3")
		plan := Plan{Children: []PlannedChild{
			{Ref: "o/r#1", Requires: []string{"Sym"}},
			{Ref: "o/r#2"},
			{Ref: "o/r#3", Produces: []string{"Sym"}},
		}}
		plan.deriveEdges(s)
		if err := plan.validate(s); err != nil {
			t.Fatalf("validate: %v (deps: %+v)", err, plan.Children)
		}
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

	t.Run("runner error stays fatal, fallback does not fire", func(t *testing.T) {
		t.Parallel()
		run := func(_ context.Context, _ string) (string, error) { return "", errors.New("boom") }
		_, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1"))
		if err == nil {
			t.Fatal("expected runner error to propagate")
		}
		if errors.Is(err, errPlannerExhausted) {
			t.Errorf("runner error must not match errPlannerExhausted (fallback must not fire): %v", err)
		}
	})

	t.Run("exhausted retries fall back to an independent-parallel plan instead of erroring", func(t *testing.T) {
		t.Parallel()
		run := func(_ context.Context, _ string) (string, error) { return "not json", nil }
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2", "o/r#3"))
		if err != nil {
			t.Fatalf("Generate: %v, want a fallback plan instead of an error", err)
		}
		if !plan.Fallback {
			t.Fatalf("Fallback = false, want true")
		}
		if plan.MaxParallel != 3 {
			t.Fatalf("MaxParallel = %d, want 3", plan.MaxParallel)
		}
		if len(plan.Children) != 3 {
			t.Fatalf("Children = %d, want 3", len(plan.Children))
		}
		depsOf := func(ref string) []string {
			for _, c := range plan.Children {
				if c.Ref == ref {
					return c.DependsOn
				}
			}
			t.Fatalf("no child %s in fallback plan", ref)
			return nil
		}
		if got := depsOf("o/r#1"); len(got) != 0 {
			t.Errorf("o/r#1 deps = %v, want none", got)
		}
		if got := depsOf("o/r#2"); len(got) != 0 {
			t.Errorf("o/r#2 deps = %v, want none", got)
		}
		if got := depsOf("o/r#3"); len(got) != 0 {
			t.Errorf("o/r#3 deps = %v, want none", got)
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
		// A persistently cyclic plan is never accepted as valid; attemptPlan
		// exhausts its retries and Generate falls back to an independent-parallel
		// plan rather than committing the cyclic plan or hard-erroring.
		cyclic := `{"children":[{"issue":"o/r#1","dependsOn":["o/r#2"]},{"issue":"o/r#2","dependsOn":["o/r#1"]}],"maxParallel":2}`
		run := func(_ context.Context, _ string) (string, error) { return cyclic, nil }
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2"))
		if err != nil {
			t.Fatalf("Generate: %v, want a fallback plan instead of an error", err)
		}
		if !plan.Fallback {
			t.Fatalf("expected the cyclic plan to be rejected in favor of a fallback: %+v", plan)
		}
	})

	t.Run("cycle derived from mutual requires/produces falls back", func(t *testing.T) {
		t.Parallel()
		// No explicit dependsOn — the cycle only exists once deriveEdges turns
		// each side's "requires" into an edge on the other's "produces". Same
		// exhaustion-then-fallback behavior as the explicit-cycle case above.
		cyclic := `{"children":[` +
			`{"issue":"o/r#1","produces":["A"],"requires":["B"]},` +
			`{"issue":"o/r#2","produces":["B"],"requires":["A"]}` +
			`],"maxParallel":2}`
		run := func(_ context.Context, _ string) (string, error) { return cyclic, nil }
		plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2"))
		if err != nil {
			t.Fatalf("Generate: %v, want a fallback plan instead of an error", err)
		}
		if !plan.Fallback {
			t.Fatalf("expected the derived cycle to be rejected in favor of a fallback: %+v", plan)
		}
	})

	// Every pair must carry a parallelJustification: under the serial-default
	// derivation, an unjustified 3-child plan is never flat (applySerialDefault
	// fills it in), so a genuinely flat plan now requires the model to have
	// justified every pair as disjoint.
	flat := `{"children":[` +
		`{"issue":"o/r#1","parallelJustification":{"o/r#2":"disjoint","o/r#3":"disjoint"}},` +
		`{"issue":"o/r#2","parallelJustification":{"o/r#3":"disjoint"}},` +
		`{"issue":"o/r#3"}` +
		`],"maxParallel":2}`
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
		if plan.Fallback {
			t.Fatalf("re-ask failure must return the original valid plan, not independentFallback: %+v", plan)
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
		if plan.Fallback {
			t.Fatalf("re-ask failure must return the original valid plan, not independentFallback: %+v", plan)
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

func TestGenerateGroundedEdge(t *testing.T) {
	t.Parallel()
	// Both children are explicitly justified parallel and report no touches
	// at all — an ungrounded run must therefore leave them fully parallel.
	plainPlan := `{"children":[` +
		`{"issue":"o/r#1","parallelJustification":{"o/r#2":"disjoint"}},` +
		`{"issue":"o/r#2","parallelJustification":{"o/r#1":"disjoint"}}` +
		`],"maxParallel":2}`
	run := func(_ context.Context, _ string) (string, error) { return plainPlan, nil }

	body1 := "This change edits `internal/foo/bar.go`."
	body2 := "This change also edits `internal/foo/bar.go`."
	s := []SubIssue{{Ref: "o/r#1", Body: body1}, {Ref: "o/r#2", Body: body2}}

	t.Run("ungrounded stays parallel", func(t *testing.T) {
		t.Parallel()
		plan, err := Generate(context.Background(), run, "o/r#100", "body", s)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		for _, c := range plan.Children {
			if len(c.DependsOn) != 0 {
				t.Fatalf("ungrounded plan should stay fully parallel, got child %s deps %v", c.Ref, c.DependsOn)
			}
		}
	})

	t.Run("grounded shared path adds a touch-overlap edge", func(t *testing.T) {
		t.Parallel()
		lister := func(_ context.Context, repo string) ([]string, error) {
			if repo != "o/r" {
				t.Fatalf("lister called with unexpected repo %q", repo)
			}
			return []string{"internal/foo/bar.go"}, nil
		}
		plan, err := Generate(context.Background(), run, "o/r#100", "body", s, WithGrounder(lister, 0))
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
			t.Fatalf("grounding a shared path should add a touch-overlap edge an ungrounded run misses, got deps %v", child2.DependsOn)
		}
	})
}

func TestIndependentFallback(t *testing.T) {
	t.Parallel()

	depsOf := func(t *testing.T, p Plan, ref string) []string {
		t.Helper()
		for _, c := range p.Children {
			if c.Ref == ref {
				return c.DependsOn
			}
		}
		t.Fatalf("no child %s in fallback plan", ref)
		return nil
	}

	t.Run("emits no DependsOn edges between children", func(t *testing.T) {
		t.Parallel()
		s := subs("o/r#3", "o/r#1", "o/r#2")
		p, err := independentFallback(s)
		if err != nil {
			t.Fatalf("independentFallback: %v", err)
		}
		for _, c := range p.Children {
			if len(c.DependsOn) != 0 {
				t.Errorf("%s deps = %v, want none", c.Ref, c.DependsOn)
			}
		}
	})

	t.Run("orders children by issue number regardless of fetch order", func(t *testing.T) {
		t.Parallel()
		p, err := independentFallback(subs("o/r#3", "o/r#1", "o/r#2"))
		if err != nil {
			t.Fatalf("independentFallback: %v", err)
		}
		var refs []string
		for _, c := range p.Children {
			refs = append(refs, c.Ref)
		}
		want := []string{"o/r#1", "o/r#2", "o/r#3"}
		if !slices.Equal(refs, want) {
			t.Errorf("child order = %v, want %v", refs, want)
		}
	})

	t.Run("sets MaxParallel to the degraded cap and marks Fallback", func(t *testing.T) {
		t.Parallel()
		p, err := independentFallback(subs("o/r#1", "o/r#2"))
		if err != nil {
			t.Fatalf("independentFallback: %v", err)
		}
		if p.MaxParallel != 3 {
			t.Errorf("MaxParallel = %d, want 3", p.MaxParallel)
		}
		if !p.Fallback {
			t.Error("Fallback = false, want true")
		}
	})

	t.Run("closed sub carries no edges either", func(t *testing.T) {
		t.Parallel()
		s := []SubIssue{
			{Ref: "o/r#1"},
			{Ref: "o/r#2", Closed: true},
			{Ref: "o/r#3"},
			{Ref: "o/r#4"},
		}
		p, err := independentFallback(s)
		if err != nil {
			t.Fatalf("independentFallback: %v", err)
		}
		if got := depsOf(t, p, "o/r#2"); len(got) != 0 {
			t.Errorf("closed o/r#2 should carry no edges, got %v", got)
		}
		if got := depsOf(t, p, "o/r#3"); len(got) != 0 {
			t.Errorf("o/r#3 deps = %v, want none", got)
		}
	})

	t.Run("covers every sub-issue exactly once", func(t *testing.T) {
		t.Parallel()
		s := subs("o/r#1", "o/r#2", "o/r#3")
		p, err := independentFallback(s)
		if err != nil {
			t.Fatalf("independentFallback: %v", err)
		}
		if err := p.validate(s); err != nil {
			t.Errorf("validate: %v", err)
		}
		if len(p.Children) != len(s) {
			t.Errorf("Children = %d, want %d", len(p.Children), len(s))
		}
	})

	t.Run("ties on issue number keep fetch order", func(t *testing.T) {
		t.Parallel()
		// o/r#1 and x/y#1 share the numeric tail "1" but are different
		// sub-issues (different repos); the tie must not reorder them.
		s := []SubIssue{{Ref: "o/r#1"}, {Ref: "x/y#1"}, {Ref: "o/r#2"}}
		p, err := independentFallback(s)
		if err != nil {
			t.Fatalf("independentFallback: %v", err)
		}
		var refs []string
		for _, c := range p.Children {
			refs = append(refs, c.Ref)
		}
		want := []string{"o/r#1", "x/y#1", "o/r#2"}
		if !slices.Equal(refs, want) {
			t.Errorf("child order = %v, want %v (tie keeps fetch order)", refs, want)
		}
	})

	t.Run("non-numeric refs keep fetch order", func(t *testing.T) {
		t.Parallel()
		s := []SubIssue{{Ref: "o/r#foo"}, {Ref: "o/r#bar"}, {Ref: "o/r#baz"}}
		p, err := independentFallback(s)
		if err != nil {
			t.Fatalf("independentFallback: %v", err)
		}
		var refs []string
		for _, c := range p.Children {
			refs = append(refs, c.Ref)
		}
		want := []string{"o/r#foo", "o/r#bar", "o/r#baz"}
		if !slices.Equal(refs, want) {
			t.Errorf("child order = %v, want %v (fetch order preserved)", refs, want)
		}
	})

	t.Run("mixed numeric and non-numeric refs still sort numeric ones by number", func(t *testing.T) {
		t.Parallel()
		// A leading non-numeric ref must not block later numeric refs from
		// being reordered into numeric order around it.
		s := []SubIssue{{Ref: "o/r#foo"}, {Ref: "o/r#3"}, {Ref: "o/r#1"}, {Ref: "o/r#2"}}
		p, err := independentFallback(s)
		if err != nil {
			t.Fatalf("independentFallback: %v", err)
		}
		var refs []string
		for _, c := range p.Children {
			refs = append(refs, c.Ref)
		}
		want := []string{"o/r#foo", "o/r#1", "o/r#2", "o/r#3"}
		if !slices.Equal(refs, want) {
			t.Errorf("child order = %v, want %v", refs, want)
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
		{
			name: "edge only on a closed child is still suspicious",
			plan: Plan{Children: []PlannedChild{
				{Ref: "o/r#1", DependsOn: []string{"o/r#2"}}, // closed child, dropped by ChildSpecs
				{Ref: "o/r#2"},
				{Ref: "o/r#3"},
				{Ref: "o/r#4"},
			}},
			subs: []SubIssue{
				{Ref: "o/r#1", Closed: true},
				{Ref: "o/r#2"},
				{Ref: "o/r#3"},
				{Ref: "o/r#4"},
			},
			want: true,
		},
		{
			name: "edge pointing only at a closed sub-issue is still suspicious",
			plan: Plan{Children: []PlannedChild{
				{Ref: "o/r#1"},
				{Ref: "o/r#2", DependsOn: []string{"o/r#1"}}, // dep already satisfied, dropped by ChildSpecs
				{Ref: "o/r#3"},
				{Ref: "o/r#4"},
			}},
			subs: []SubIssue{
				{Ref: "o/r#1", Closed: true},
				{Ref: "o/r#2"},
				{Ref: "o/r#3"},
				{Ref: "o/r#4"},
			},
			want: true,
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
