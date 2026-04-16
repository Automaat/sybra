package workflow

import "testing"

func TestEvalCondition(t *testing.T) {
	fields := map[string]string{
		"task.status": "in-progress",
		"task.tags":   "backend,auth",
		"empty":       "",
	}

	tests := []struct {
		name string
		cond Condition
		want bool
	}{
		{
			name: "equals matches",
			cond: Condition{Field: "task.status", Operator: "equals", Value: "in-progress"},
			want: true,
		},
		{
			name: "equals rejects mismatch",
			cond: Condition{Field: "task.status", Operator: "equals", Value: "done"},
			want: false,
		},
		{
			name: "not_equals matches on different value",
			cond: Condition{Field: "task.status", Operator: "not_equals", Value: "done"},
			want: true,
		},
		{
			name: "not_equals rejects same value",
			cond: Condition{Field: "task.status", Operator: "not_equals", Value: "in-progress"},
			want: false,
		},
		{
			name: "contains finds substring",
			cond: Condition{Field: "task.tags", Operator: "contains", Value: "auth"},
			want: true,
		},
		{
			name: "contains rejects missing substring",
			cond: Condition{Field: "task.tags", Operator: "contains", Value: "frontend"},
			want: false,
		},
		{
			name: "not_contains passes when substring absent",
			cond: Condition{Field: "task.tags", Operator: "not_contains", Value: "frontend"},
			want: true,
		},
		{
			name: "not_contains fails when substring present",
			cond: Condition{Field: "task.tags", Operator: "not_contains", Value: "auth"},
			want: false,
		},
		{
			name: "exists for present field",
			cond: Condition{Field: "task.status", Operator: "exists"},
			want: true,
		},
		{
			name: "exists for absent field",
			cond: Condition{Field: "missing.field", Operator: "exists"},
			want: false,
		},
		{
			name: "exists for empty string field",
			cond: Condition{Field: "empty", Operator: "exists"},
			want: true,
		},
		{
			name: "unknown operator returns false",
			cond: Condition{Field: "task.status", Operator: "greater_than", Value: "0"},
			want: false,
		},
		{
			name: "equals against absent field",
			cond: Condition{Field: "missing", Operator: "equals", Value: ""},
			want: true, // absent field has zero-value ""
		},
		{
			name: "in matches first csv entry",
			cond: Condition{Field: "task.status", Operator: "in", Value: "in-progress,done"},
			want: true,
		},
		{
			name: "in matches with surrounding whitespace",
			cond: Condition{Field: "task.status", Operator: "in", Value: "todo , in-progress , done"},
			want: true,
		},
		{
			name: "in rejects value not listed",
			cond: Condition{Field: "task.status", Operator: "in", Value: "todo,done"},
			want: false,
		},
		{
			name: "not_in passes when value not listed",
			cond: Condition{Field: "task.status", Operator: "not_in", Value: "todo,done"},
			want: true,
		},
		{
			name: "not_in rejects when value listed",
			cond: Condition{Field: "task.status", Operator: "not_in", Value: "in-progress,done"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvalCondition(tt.cond, fields)
			if got != tt.want {
				t.Errorf("EvalCondition(%+v) = %v, want %v", tt.cond, got, tt.want)
			}
		})
	}
}

func TestEvalConditions_AllMustMatch(t *testing.T) {
	fields := map[string]string{
		"task.status": "todo",
		"task.tags":   "backend,small",
	}

	tests := []struct {
		name       string
		conditions []Condition
		want       bool
	}{
		{
			name:       "empty conditions returns true",
			conditions: nil,
			want:       true,
		},
		{
			name: "single matching condition",
			conditions: []Condition{
				{Field: "task.status", Operator: "equals", Value: "todo"},
			},
			want: true,
		},
		{
			name: "all conditions match",
			conditions: []Condition{
				{Field: "task.status", Operator: "equals", Value: "todo"},
				{Field: "task.tags", Operator: "contains", Value: "backend"},
			},
			want: true,
		},
		{
			name: "one condition fails rejects all",
			conditions: []Condition{
				{Field: "task.status", Operator: "equals", Value: "todo"},
				{Field: "task.tags", Operator: "contains", Value: "frontend"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvalConditions(tt.conditions, fields)
			if got != tt.want {
				t.Errorf("EvalConditions() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEvalCondition_UndefinedField pins the "missing field" semantics: each
// operator treats absent fields as the zero value ("") rather than raising
// an error. This is intentional — workflow authors can write conditions
// against optional fields without guarding — but the exact behavior per
// operator is non-obvious and worth locking down.
//
//   - equals ""       → true  (absent == "")
//   - equals "anything"→ false (absent != "anything")
//   - not_equals "x"  → true  ("" != "x")
//   - contains ""     → true  (every string contains empty substring)
//   - contains "x"    → false
//   - exists          → false (presence-check)
//   - in "a,b"        → false (zero value not in list)
//   - in ",a,b"       → true  (empty string IS in the csv-list containing "")
//
// A regression that flipped any of these would silently change which
// workflow branches fire — surface it here rather than at runtime.
func TestEvalCondition_UndefinedField(t *testing.T) {
	t.Parallel()
	fields := map[string]string{} // nothing defined

	cases := []struct {
		op, val string
		want    bool
	}{
		{"equals", "", true},
		{"equals", "x", false},
		{"not_equals", "", false},
		{"not_equals", "x", true},
		{"contains", "", true},
		{"contains", "x", false},
		{"not_contains", "x", true},
		{"not_contains", "", false},
		{"exists", "", false},
		{"exists", "x", false},
		{"in", "a,b,c", false},
		{"in", ",a,b", true}, // empty entry in csv matches zero-value field
		{"not_in", "a,b,c", true},
		{"unknown_op", "x", false},
	}
	for _, c := range cases {
		t.Run(c.op+"_"+c.val, func(t *testing.T) {
			t.Parallel()
			got := EvalCondition(Condition{Field: "vars.missing", Operator: c.op, Value: c.val}, fields)
			if got != c.want {
				t.Errorf("EvalCondition(%s %q) on missing field = %v, want %v", c.op, c.val, got, c.want)
			}
		})
	}
}

// TestEvalCondition_UnicodeValues documents behavior under non-ASCII input:
// contains/equals compare raw bytes, so NFC-vs-NFD variants of the same
// user-visible character compare as different. A workflow author who writes
// `contains "café"` (NFC) will not match an agent output that emits "café"
// (NFD). The test pins this, so if future work adds normalization the
// assertions flip explicitly.
func TestEvalCondition_UnicodeValues(t *testing.T) {
	t.Parallel()
	nfc := "caf\u00e9"  // precomposed é
	nfd := "cafe\u0301" // e + combining acute
	fields := map[string]string{"task.title": nfc}

	if !EvalCondition(Condition{Field: "task.title", Operator: "equals", Value: nfc}, fields) {
		t.Error("NFC equals NFC should be true")
	}
	if EvalCondition(Condition{Field: "task.title", Operator: "equals", Value: nfd}, fields) {
		t.Error("NFC equals NFD currently returns true — normalization was added; update test")
	}
	if !EvalCondition(Condition{Field: "task.title", Operator: "contains", Value: "caf"}, fields) {
		t.Error("ASCII prefix 'caf' should be found in NFC 'café'")
	}
}

func TestResolveTransition(t *testing.T) {
	fields := map[string]string{
		"task.status":       "planning",
		"vars.human_action": "approve",
	}

	tests := []struct {
		name        string
		transitions []Transition
		wantID      string
		wantErr     bool
	}{
		{
			name:        "empty transitions returns empty",
			transitions: nil,
			wantID:      "",
		},
		{
			name: "unconditional fallback",
			transitions: []Transition{
				{GoTo: "next_step"},
			},
			wantID: "next_step",
		},
		{
			name: "first matching conditional wins",
			transitions: []Transition{
				{When: &Condition{Field: "task.status", Operator: "equals", Value: "planning"}, GoTo: "plan"},
				{GoTo: "default"},
			},
			wantID: "plan",
		},
		{
			name: "falls through to default when conditional misses",
			transitions: []Transition{
				{When: &Condition{Field: "task.status", Operator: "equals", Value: "done"}, GoTo: "done"},
				{GoTo: "default"},
			},
			wantID: "default",
		},
		{
			name: "goto empty string ends workflow",
			transitions: []Transition{
				{GoTo: ""},
			},
			wantID: "",
		},
		{
			name: "no matching transition with conditionals only returns error",
			transitions: []Transition{
				{When: &Condition{Field: "task.status", Operator: "equals", Value: "done"}, GoTo: "done"},
				{When: &Condition{Field: "task.status", Operator: "equals", Value: "failed"}, GoTo: "fail"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, err := ResolveTransition(tt.transitions, fields)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotID != tt.wantID {
				t.Errorf("got %q, want %q", gotID, tt.wantID)
			}
		})
	}
}
