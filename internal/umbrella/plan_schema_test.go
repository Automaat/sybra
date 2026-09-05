package umbrella

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func planSchemaFor(t *testing.T, refs ...string) map[string]any {
	t.Helper()
	subs := make([]SubIssue, len(refs))
	for i, r := range refs {
		subs[i] = SubIssue{Ref: r}
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(buildPlanSchema(subs)), &schema); err != nil {
		t.Fatalf("buildPlanSchema is not valid JSON: %v", err)
	}
	return schema
}

func TestBuildPlanSchema_JustificationIsAPairArray(t *testing.T) {
	// Given the child schema
	schema := planSchemaFor(t, "o/r#1", "o/r#2")
	children := schema["properties"].(map[string]any)["children"].(map[string]any)
	child := children["items"].(map[string]any)
	props := child["properties"].(map[string]any)

	// When parallelJustification is inspected
	just, ok := props["parallelJustification"].(map[string]any)
	if !ok {
		t.Fatalf("parallelJustification missing from the child schema: %v", props)
	}

	// Then it is an array of sibling/reason pairs, never an open map
	if just["type"] != "array" {
		t.Fatalf("parallelJustification type = %v, want array", just["type"])
	}
	item, ok := just["items"].(map[string]any)
	if !ok {
		t.Fatalf("parallelJustification items = %T, want a schema object", just["items"])
	}
	if _, schemaValued := item["additionalProperties"].(map[string]any); schemaValued {
		t.Fatal("pair schema uses a schema-valued additionalProperties, which the API refuses")
	}
	pairProps := item["properties"].(map[string]any)
	for _, key := range []string{"sibling", "reason"} {
		if _, present := pairProps[key]; !present {
			t.Fatalf("pair schema is missing %q: %v", key, pairProps)
		}
	}
}

func TestParallelJustification_AcceptsBothWireForms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want ParallelJustification
	}{
		{"object map", `{"o/r#2":"disjoint files"}`, ParallelJustification{"o/r#2": "disjoint files"}},
		{"pair array", `[{"sibling":"o/r#2","reason":"disjoint files"}]`, ParallelJustification{"o/r#2": "disjoint files"}},
		{"empty array", `[]`, nil},
		{"null", `null`, nil},
		{"blank sibling dropped", `[{"sibling":"  ","reason":"x"}]`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var child PlannedChild
			if err := json.Unmarshal([]byte(`{"issue":"o/r#1","parallelJustification":`+tc.raw+`}`), &child); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tc.want == nil && child.ParallelJustification != nil {
				t.Fatalf("ParallelJustification = %#v, want nil", child.ParallelJustification)
			}
			if len(child.ParallelJustification) != len(tc.want) {
				t.Fatalf("ParallelJustification = %v, want %v", child.ParallelJustification, tc.want)
			}
			for k, v := range tc.want {
				if child.ParallelJustification[k] != v {
					t.Fatalf("ParallelJustification[%q] = %q, want %q", k, child.ParallelJustification[k], v)
				}
			}
		})
	}
}

func walkSchemaNodes(t *testing.T, node any, path string, visit func(path string, obj map[string]any)) {
	t.Helper()
	switch typed := node.(type) {
	case map[string]any:
		visit(path, typed)
		for key, child := range typed {
			walkSchemaNodes(t, child, path+"."+key, visit)
		}
	case []any:
		for i, child := range typed {
			walkSchemaNodes(t, child, fmt.Sprintf("%s[%d]", path, i), visit)
		}
	}
}

func TestBuildPlanSchema_NoRefusedConstructAtAnyDepth(t *testing.T) {
	// Given the planner schema for an umbrella
	schema := planSchemaFor(t, "o/r#1", "o/r#2", "o/r#3")

	// When every node in it is walked
	walkSchemaNodes(t, schema, "$", func(path string, obj map[string]any) {
		// Then no node carries a construct the structured-output API refuses
		for _, banned := range []string{"uniqueItems", "additionalItems", "patternProperties"} {
			if _, present := obj[banned]; present {
				t.Errorf("%s carries %q, which the structured-output API refuses", path, banned)
			}
		}
		if _, isTuple := obj["items"].([]any); isTuple {
			t.Errorf("%s.items is a tuple; the API requires a single schema", path)
		}
		if _, schemaValued := obj["additionalProperties"].(map[string]any); schemaValued {
			t.Errorf("%s.additionalProperties is schema-valued; the API requires false", path)
		}
	})
}

func TestGenerate_CorrectiveRetryRecoversMismatchedCoverage(t *testing.T) {
	t.Parallel()
	// Given a planner whose first answer duplicates one sub-issue and omits another
	var prompts []string
	calls := 0
	run := func(_ context.Context, prompt, _ string) (string, error) {
		calls++
		prompts = append(prompts, prompt)
		if calls == 1 {
			return `{"children":[{"issue":"o/r#1"},{"issue":"o/r#1"},{"issue":"o/r#2"}],"maxParallel":3}`, nil
		}
		return `{"children":[{"issue":"o/r#1"},{"issue":"o/r#2","parallelJustification":[{"sibling":"o/r#1","reason":"disjoint packages"}]},{"issue":"o/r#3"}],"maxParallel":3}`, nil
	}

	// When the plan is generated
	plan, err := Generate(context.Background(), run, "o/r#100", "body", subs("o/r#1", "o/r#2", "o/r#3"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Then the mismatch was re-asked rather than accepted, keeping the pair justification
	if calls < 2 {
		t.Fatalf("planner calls = %d, want a corrective re-ask after mismatched coverage", calls)
	}
	if plan.Fallback {
		t.Fatalf("plan fell back to the degraded chain: %+v", plan)
	}
	if len(plan.Children) != 3 {
		t.Fatalf("plan covers %d children, want 3", len(plan.Children))
	}
	if len(prompts) < 2 {
		t.Fatalf("captured %d prompts, want the corrective re-ask", len(prompts))
	}
	corrective := strings.Join(prompts[1:], "\n")
	for _, ref := range []string{"o/r#1", "o/r#3"} {
		if !strings.Contains(corrective, ref) {
			t.Fatalf("corrective prompt never named %s:\n%s", ref, corrective)
		}
	}
	for _, child := range plan.Children {
		if child.Ref == "o/r#2" && child.ParallelJustification["o/r#1"] == "" {
			t.Fatalf("justification lost for %s: %+v", child.Ref, child)
		}
	}
}
