package task

import (
	"encoding/json"
	"testing"
)

func TestApplyMapFields_JSONDecodedArrays(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    []string
		field   string
		wantErr bool
	}{
		{name: "tags from json array", body: `{"tags":["a","b"]}`, field: "tags", want: []string{"a", "b"}},
		{name: "tags empty json array", body: `{"tags":[]}`, field: "tags", want: []string{}},
		{name: "tags comma string", body: `{"tags":"a,b"}`, field: "tags", want: []string{"a", "b"}},
		{name: "tags non-string element", body: `{"tags":["a",7]}`, field: "tags", wantErr: true},
		{name: "depends_on from json array", body: `{"depends_on":["t1","t2"]}`, field: "depends_on", want: []string{"t1", "t2"}},
		{name: "depends_on non-string element", body: `{"depends_on":[{"x":1}]}`, field: "depends_on", wantErr: true},
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
			got := u.Tags
			if tt.field == "depends_on" {
				got = u.DependsOn
			}
			if got == nil {
				t.Fatalf("%s not set", tt.field)
			}
			if len(*got) != len(tt.want) {
				t.Fatalf("%s = %v, want %v", tt.field, *got, tt.want)
			}
			for i := range tt.want {
				if (*got)[i] != tt.want[i] {
					t.Fatalf("%s = %v, want %v", tt.field, *got, tt.want)
				}
			}
		})
	}
}
