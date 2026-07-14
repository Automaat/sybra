package workflow

import "testing"

func TestPlanHasOpenDecisions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "canonical no open decisions",
			in:   "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.",
			want: false,
		},
		{
			name: "empty is conservative",
			in:   "",
			want: true,
		},
		{
			name: "decision heading",
			in:   "# Decisions\n\n## Scope\nQuestion: Which scope?\nRecommended: A\n\nOptions:\n- A - one\n- B - two",
			want: true,
		},
		{
			name: "ambiguous text without explicit no-open marker",
			in:   "# Decisions\n\nAll set.",
			want: true,
		},
		{
			name: "copied instruction marker is not enough",
			in:   "# Decisions\n\nIf there are no meaningful choices, write: No open decisions.\n\nNo open decisions.",
			want: true,
		},
		{
			name: "marker plus structured decision is open",
			in:   "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.\n\n## Scope\nQuestion: Which scope?",
			want: true,
		},
		{
			name: "marker plus decision labels is open",
			in:   "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.\n\nRecommended: Small\nOptions:\n- Small - minimal",
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PlanHasOpenDecisions(tc.in); got != tc.want {
				t.Errorf("PlanHasOpenDecisions() = %v, want %v", got, tc.want)
			}
		})
	}
}
