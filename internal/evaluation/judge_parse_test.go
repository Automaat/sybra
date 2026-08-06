package evaluation

import "testing"

// The judge's JSON arrives wrapped in prose and sometimes inside the claude
// `--output-format json` envelope, so this call site depends on the shared
// scanner's two hard cases: a brace inside a string value, and a decoy object
// emitted before the want answer.
func TestParseQualityVerdictResolvesTheRealObject(t *testing.T) {
	t.Parallel()
	want := `{"overall":8,"dimensions":{"correctness":{"score":8},"code_quality":{"score":8},"scope_discipline":{"score":8},"test_coverage":{"score":8},"review_worthiness":{"score":8}},"summary":"solid"}`

	tests := []struct {
		name string
		raw  string
	}{
		{name: "bare object", raw: want},
		{name: "prose around", raw: "My verdict:\n" + want + "\nend."},
		{name: "fenced", raw: "```json\n" + want + "\n```"},
		{
			name: "close brace inside a string value",
			raw:  `{"overall":1,"dimensions":{"correctness":{"score":8},"code_quality":{"score":8},"scope_discipline":{"score":8},"test_coverage":{"score":8},"review_worthiness":{"score":8}},"summary":"a } in the diff"}` + "\nrevised:\n" + want,
		},
		{
			name: "decoy object first",
			raw:  `{"overall":1,"dimensions":{"correctness":{"score":8},"code_quality":{"score":8},"scope_discipline":{"score":8},"test_coverage":{"score":8},"review_worthiness":{"score":8}},"summary":"draft"}` + "\nOn reflection:\n" + want,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseQualityVerdict([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parseQualityVerdict: %v", err)
			}
			if got.Overall != 8 || got.Summary != "solid" {
				t.Errorf("resolved the wrong object: %+v", got)
			}
		})
	}
}

func TestParseQualityVerdictRejectsOutputWithNoObject(t *testing.T) {
	t.Parallel()
	if _, err := parseQualityVerdict([]byte("the judge refused")); err == nil {
		t.Error("expected an error on output containing no JSON object")
	}
}
