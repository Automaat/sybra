package prreview

import "testing"

func TestIs(t *testing.T) {
	tests := []struct {
		name     string
		taskID   string
		branch   string
		prNumber int
		tags     []string
		want     bool
	}{
		{"another author's branch", "b3c276be", "feat/theirs", 382, []string{Tag}, true},
		{"no branch yet", "b3c276be", "", 382, []string{Tag}, true},
		{"branch minted for this task", "b3c276be", "chore/review-their-work-b3c276be", 382, []string{Tag}, true},
		{"branch minted for another task", "aa11bb22", "fix/adopted-orphan-696bc049", 8, []string{Tag}, false},
		{"no review tag", "b3c276be", "feat/theirs", 382, []string{"backend"}, false},
		{"no pull request", "b3c276be", "feat/theirs", 0, []string{Tag}, false},
		{"suffix is not hex", "b3c276be", "feat/release-2026zzzz", 382, []string{Tag}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Is(tc.taskID, tc.branch, tc.prNumber, tc.tags); got != tc.want {
				t.Fatalf("Is() = %v, want %v", got, tc.want)
			}
		})
	}
}
