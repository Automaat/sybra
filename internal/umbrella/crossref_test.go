package umbrella

import (
	"reflect"
	"testing"
)

func TestExternalBlockers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		selfRef string
		want    []string
	}{
		{
			name: "strictly after bare ref",
			body: "This must land strictly after #2464 merges in the other program.",
			want: []string{"#2464"},
		},
		{
			name: "after bare ref",
			body: "Do this after #12 lands.",
			want: []string{"#12"},
		},
		{
			name: "after qualified ref",
			body: "Blocked after Automaat/other-repo#45 ships.",
			want: []string{"automaat/other-repo#45"},
		},
		{
			name:    "self reference excluded",
			body:    "Strictly after #99, which is this very issue.",
			selfRef: "Automaat/sybra#99",
			want:    nil,
		},
		{
			name: "deduplicated",
			body: "Strictly after #7. Also after #7 again.",
			want: []string{"#7"},
		},
		{
			name: "no match without a following issue ref",
			body: "We'll fix this shortly afterward, right after lunch.",
			want: nil,
		},
		{
			name: "no match on empty body",
			body: "",
			want: nil,
		},
		{
			name: "case insensitive",
			body: "STRICTLY AFTER #5 is done.",
			want: []string{"#5"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := ExternalBlockers(c.body, c.selfRef)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ExternalBlockers(%q, %q) = %v, want %v", c.body, c.selfRef, got, c.want)
			}
		})
	}
}

func TestGraph_UnresolvedRefs(t *testing.T) {
	t.Parallel()
	g := Build([]Node{
		{ID: "done-task", Issue: "Automaat/sybra#1", Done: true},
		{ID: "open-task", Issue: "Automaat/sybra#2", Done: false},
	})

	got := g.UnresolvedRefs([]string{"Automaat/sybra#1", "Automaat/sybra#2", "#2464"})
	want := []string{"Automaat/sybra#2", "#2464"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UnresolvedRefs() = %v, want %v", got, want)
	}

	if got := g.UnresolvedRefs([]string{"Automaat/sybra#1"}); got != nil {
		t.Errorf("UnresolvedRefs(done only) = %v, want nil", got)
	}
}
