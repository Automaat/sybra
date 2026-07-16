package stats

import "testing"

// IsTerminalOutcome is an allowlist on purpose: a denylist ("anything but
// stalled") would count an empty, corrupt, or newly-added outcome as a resolved
// non-failure and quietly skew every failure-rate denominator that reads it.
func TestIsTerminalOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		outcome string
		want    bool
	}{
		{OutcomeCompleted, true},
		{OutcomeFailed, true},
		{OutcomeStalled, false},
		{"", false},
		{"some-future-outcome", false},
		{"COMPLETED", false},
	}
	for _, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			t.Parallel()
			if got := IsTerminalOutcome(tc.outcome); got != tc.want {
				t.Errorf("IsTerminalOutcome(%q) = %v, want %v", tc.outcome, got, tc.want)
			}
		})
	}
}
