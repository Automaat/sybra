package triage

import "testing"

func TestParseVerdict(t *testing.T) {
	raw := []byte(`{"result":"Here is the verdict:\n{\"title\":\"feat(api): add auth\",\"original_title\":\"i want auth\",\"description\":\"\",\"tags\":[\"backend\",\"medium\",\"feature\"],\"size\":\"medium\",\"type\":\"feature\",\"mode\":\"headless\",\"project_id\":\"\"}"}`)
	v, err := parseVerdict(raw)
	if err != nil {
		t.Fatalf("parseVerdict: %v", err)
	}
	if v.Title != "feat(api): add auth" {
		t.Errorf("title: got %q", v.Title)
	}
	if v.Size != "medium" || v.Type != "feature" || v.Mode != "headless" {
		t.Errorf("bad fields: %+v", v)
	}
	if err := ValidateVerdict(&v); err != nil {
		t.Errorf("validate: %v", err)
	}
}

func TestParseVerdictEmptyResult(t *testing.T) {
	raw := []byte(`{"result":""}`)
	if _, err := parseVerdict(raw); err == nil {
		t.Errorf("expected error on empty result")
	}
}

func TestParseVerdictNoJSON(t *testing.T) {
	raw := []byte(`{"result":"no json here just prose"}`)
	if _, err := parseVerdict(raw); err == nil {
		t.Errorf("expected error on missing JSON")
	}
}

func TestExtractLastJSONObject(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{`prose {"a":1} more prose`, `{"a":1}`},
		{`{"a":1} then {"b":2}`, `{"b":2}`},
		{`{"s":"}{"}`, `{"s":"}{"}`},
		{`no json`, ``},
	}
	for _, tc := range tests {
		got := extractLastJSONObject(tc.in)
		if got != tc.want {
			t.Errorf("extract(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
