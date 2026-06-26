package umbrella

import "testing"

func TestIsUmbrellaIssue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		title  string
		labels []string
		want   bool
	}{
		{"umbrella emoji prefix", "☂️ feat(x): umbrella", nil, true},
		{"bare umbrella rune (no VS16)", "☂ feat(x): umbrella", nil, true},
		{"emoji with leading space", "  ☂️ leading space", nil, true},
		{"umbrella label", "ordinary title", []string{"bug", "umbrella"}, true},
		{"emoji mid-title is not a prefix", "feat ☂️ inline", nil, false},
		{"plain issue", "ordinary title", []string{"bug"}, false},
		{"no title no labels", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := IsUmbrellaIssue(c.title, c.labels); got != c.want {
				t.Errorf("IsUmbrellaIssue(%q, %v) = %v, want %v", c.title, c.labels, got, c.want)
			}
		})
	}
}
