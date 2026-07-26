package task

import (
	"encoding/json"
	"testing"
)

func TestApplyDependsOnConditionsField(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    []DepCondition
		wantErr bool
	}{
		{
			name: "label and note conditions",
			body: `{"depends_on_conditions":[{"ref":"o/r#1","kind":"label","value":"scope-confirmed"},{"ref":"o/r#2","kind":"note","value":"confirm perms"}]}`,
			want: []DepCondition{
				{Ref: "o/r#1", Kind: DepConditionKindLabel, Value: "scope-confirmed"},
				{Ref: "o/r#2", Kind: DepConditionKindNote, Value: "confirm perms"},
			},
		},
		{
			name: "empty array clears",
			body: `{"depends_on_conditions":[]}`,
			want: []DepCondition{},
		},
		{
			name:    "unknown kind rejected",
			body:    `{"depends_on_conditions":[{"ref":"o/r#1","kind":"pr-merged","value":"x"}]}`,
			wantErr: true,
		},
		{
			name:    "missing ref rejected",
			body:    `{"depends_on_conditions":[{"ref":"","kind":"note","value":"x"}]}`,
			wantErr: true,
		},
		{
			name:    "missing value rejected",
			body:    `{"depends_on_conditions":[{"ref":"o/r#1","kind":"note","value":""}]}`,
			wantErr: true,
		},
		{
			name:    "duplicate ref rejected",
			body:    `{"depends_on_conditions":[{"ref":"o/r#1","kind":"note","value":"a"},{"ref":"o/r#1","kind":"label","value":"b"}]}`,
			wantErr: true,
		},
		{
			// URL and shorthand normalize to the same ref (issueref.Normalize),
			// matching matchesDepRef's gate-time equivalence — must be rejected
			// as a duplicate, not silently accepted as two distinct refs.
			name:    "duplicate ref url vs shorthand rejected",
			body:    `{"depends_on_conditions":[{"ref":"o/r#1","kind":"note","value":"a"},{"ref":"https://github.com/o/r/issues/1","kind":"label","value":"b"}]}`,
			wantErr: true,
		},
		{
			name:    "non-object element rejected",
			body:    `{"depends_on_conditions":["not-an-object"]}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal([]byte(tt.body), &m); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}

			u, err := UpdateFromMap(m)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("UpdateFromMap(%s) = %+v, want error", tt.body, u)
				}
				return
			}
			if err != nil {
				t.Fatalf("UpdateFromMap(%s): %v", tt.body, err)
			}
			if u.DependsOnConditions == nil {
				t.Fatalf("DependsOnConditions not set")
			}
			got := *u.DependsOnConditions
			if len(got) != len(tt.want) {
				t.Fatalf("DependsOnConditions = %+v, want %+v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("DependsOnConditions[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
