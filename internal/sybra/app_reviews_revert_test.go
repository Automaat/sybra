package sybra

import "testing"

func TestIsReverted(t *testing.T) {
	t.Parallel()
	sha := "abc123def456abc123def456abc123def456abcd"
	tests := []struct {
		name string
		sha  string
		repo string
		pr   int
		msgs []string
		want bool
	}{
		{"footer full sha", sha, "o/r", 42, []string{"Revert \"feat: x\"", "This reverts commit " + sha + "."}, true},
		{"pr-number body form", "", "o/r", 42, []string{"Revert \"feat: x\" (#99)\n\nReverts #42"}, true},
		{"qualified pr-number form", "", "o/r", 42, []string{"Reverts o/r#42"}, true},
		{"not reverted", sha, "o/r", 42, []string{"feat: unrelated", "fix: other"}, false},
		{"passing mention of pr not a revert", "", "o/r", 42, []string{"see #42 for context"}, false},
		{"no sha and no pr", "", "o/r", 0, []string{"This reverts commit ."}, false},
		{"different sha", sha, "o/r", 7, []string{"This reverts commit 0000000000000000000000000000000000000000."}, false},
		{"no messages", sha, "o/r", 42, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isReverted(tt.sha, tt.repo, tt.pr, tt.msgs); got != tt.want {
				t.Errorf("isReverted = %v, want %v", got, tt.want)
			}
		})
	}
}
