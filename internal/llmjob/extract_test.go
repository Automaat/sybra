package llmjob

import "testing"

func TestExtractLastJSONObject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain object", in: `{"a":1}`, want: `{"a":1}`},
		{name: "prose around", in: "sure:\n{\"a\":1}\ndone", want: `{"a":1}`},
		{name: "fenced code block", in: "```json\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "nested objects", in: `{"a":{"b":2}}`, want: `{"a":{"b":2}}`},
		{name: "no object", in: "no json here", want: ""},
		{name: "unclosed object", in: `prose {"a":1`, want: ""},

		// A naive first-brace/last-brace scan mis-slices these. Both shapes
		// come out of real judge and planner runs.
		{name: "close brace inside a string value", in: `{"a":"}"}`, want: `{"a":"}"}`},
		{name: "escaped quote before a brace", in: `{"a":"x\"}y"}`, want: `{"a":"x\"}y"}`},
		{name: "open brace inside a string value", in: `{"a":"{"}`, want: `{"a":"{"}`},
		{name: "prose containing braces", in: `use { and } freely {"a":1}`, want: `{"a":1}`},

		// A model that reasons before answering emits its real answer last.
		{name: "decoy object first", in: `{"draft":true}` + "\nactually:\n" + `{"final":true}`, want: `{"final":true}`},
		{name: "decoy inside a fence", in: "```\n{\"draft\":1}\n```\nfinal answer:\n{\"final\":2}", want: `{"final":2}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractLastJSONObject(tt.in); got != tt.want {
				t.Errorf("ExtractLastJSONObject(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
