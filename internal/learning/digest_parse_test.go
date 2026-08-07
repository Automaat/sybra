package learning

import "testing"

// The summarizer wraps its JSON in prose, so this call site depends on the
// shared scanner's two hard cases: a brace inside a string value, and a decoy
// object emitted before the want answer.
func TestParseDigestJSONResolvesTheRealObject(t *testing.T) {
	t.Parallel()
	want := `{"since":"2026-01-01","until":"2026-01-07","worked":["shipped the gate"]}`

	tests := []struct {
		name string
		text string
	}{
		{name: "bare object", text: want},
		{name: "prose around", text: "Here is the digest:\n" + want + "\ndone."},
		{name: "fenced", text: "```json\n" + want + "\n```"},
		{
			name: "close brace inside a string value",
			text: `{"since":"x","until":"y","worked":["a } in the diff"]}` + "\nrevised:\n" + want,
		},
		{
			name: "decoy object first",
			text: `{"since":"draft","until":"draft","worked":["wrong"]}` + "\nOn reflection:\n" + want,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDigestJSON(tt.text)
			if err != nil {
				t.Fatalf("parseDigestJSON: %v", err)
			}
			if got.Since != "2026-01-01" || got.Until != "2026-01-07" {
				t.Errorf("resolved the wrong object: %+v", got)
			}
			if len(got.Worked) != 1 || got.Worked[0] != "shipped the gate" {
				t.Errorf("worked = %v, want the want object's entry", got.Worked)
			}
		})
	}
}

func TestParseDigestJSONRejectsOutputWithNoObject(t *testing.T) {
	t.Parallel()
	if _, err := parseDigestJSON("the summarizer refused"); err == nil {
		t.Error("expected an error on output containing no JSON object")
	}
}
