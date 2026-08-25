package prreview

import "testing"

func TestIs(t *testing.T) {
	tests := []struct {
		name     string
		prNumber int
		tags     []string
		want     bool
	}{
		{"review of another author's pull request", 382, []string{Tag}, true},
		{"adopted own pull request", 8, []string{Tag, TagAdopted}, false},
		{"no review tag", 382, []string{"backend"}, false},
		{"no pull request", 0, []string{Tag}, false},
		{"adopted marker without review tag", 8, []string{TagAdopted}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Is(tc.prNumber, tc.tags); got != tc.want {
				t.Fatalf("Is() = %v, want %v", got, tc.want)
			}
		})
	}
}
