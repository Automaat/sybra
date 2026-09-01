package umbrella

import (
	"encoding/json"
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
