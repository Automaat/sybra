package umbrella

import "testing"

func TestChildSpecs(t *testing.T) {
	t.Parallel()
	plan := Plan{Children: []PlannedChild{
		{Ref: "o/r#1", DependsOn: nil, Track: "A"},
		{Ref: "o/r#2", DependsOn: []string{"o/r#1"}, Track: "A"},
		{Ref: "o/r#3", DependsOn: []string{"o/r#1"}, Track: "B"},
	}}
	ss := []SubIssue{
		{Ref: "o/r#1", Title: "first", Body: "b1"},
		{Ref: "o/r#2", Title: "second", Body: "b2"},
		{Ref: "o/r#3", Title: "third", Body: "b3"},
	}

	t.Run("creates all when none exist", func(t *testing.T) {
		t.Parallel()
		specs := ChildSpecs(plan, ss, nil)
		if len(specs) != 3 {
			t.Fatalf("got %d specs, want 3", len(specs))
		}
		if specs[0].Title != "first" || specs[0].Body != "b1" || specs[0].Issue != "o/r#1" {
			t.Errorf("spec[0] not populated from sub-issue: %+v", specs[0])
		}
		if specs[1].DependsOn[0] != "o/r#1" {
			t.Errorf("spec[1] deps = %v", specs[1].DependsOn)
		}
	})

	t.Run("idempotent: skips already-materialized refs", func(t *testing.T) {
		t.Parallel()
		// #1 and #2 already have tasks (refs written in different forms).
		existing := map[string]bool{
			NormalizeIssueRef("https://github.com/o/r/issues/1"): true,
			NormalizeIssueRef("o/r#2"):                           true,
		}
		specs := ChildSpecs(plan, ss, existing)
		if len(specs) != 1 || specs[0].Issue != "o/r#3" {
			t.Fatalf("re-expansion should only create #3, got %+v", specs)
		}
	})
}
