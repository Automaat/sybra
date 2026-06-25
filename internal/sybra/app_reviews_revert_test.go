package sybra

import "testing"

func TestIsReverted(t *testing.T) {
	t.Parallel()
	sha := "abc123def456abc123def456abc123def456abcd"
	tests := []struct {
		name string
		sha  string
		msgs []string
		want bool
	}{
		{"reverted full sha", sha, []string{"Revert \"feat: x\"", "This reverts commit " + sha + "."}, true},
		{"not reverted", sha, []string{"feat: unrelated", "fix: other"}, false},
		{"empty sha never reverted", "", []string{"This reverts commit ."}, false},
		{"different sha", sha, []string{"This reverts commit 0000000000000000000000000000000000000000."}, false},
		{"no messages", sha, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isReverted(tt.sha, tt.msgs); got != tt.want {
				t.Errorf("isReverted = %v, want %v", got, tt.want)
			}
		})
	}
}
