package umbrella

import (
	"context"
	"errors"
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
	// The planner must be told to serialize sub-issues that edit the same files,
	// so overlapping siblings merge one at a time instead of colliding.
	for _, want := range []string{"SAME files", "merge one at a time", "disjoint"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing serialization guidance %q:\n%s", want, prompt)
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

func TestGenerate(t *testing.T) {
	t.Parallel()
	good := `{"children":[{"issue":"o/r#1","dependsOn":[]},{"issue":"o/r#2","dependsOn":["o/r#1"]}],"maxParallel":2}`

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
}
