package watchdogreason

import "testing"

func TestIsRetryableStop(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   bool
	}{
		{"legacy bare stop", "watchdog stop", true},
		{"natural watchdog reason", "watchdog: repeating command", true},
		{"reward hacking kind", "watchdog: reward_hacking", false},
		{"reward hacking kind with detail", "watchdog: reward_hacking: repeated fake progress", false},
		{"rate limit", "watchdog: rate limit: quota exhausted", false},
		{"verify failed", "watchdog: verify suite still fails after loop stop: go test ./...", false},
		{"verify unconfirmed", "watchdog: could not confirm agent stopped before verify - watchdog stop", false},
		{"retry budget exhausted", "watchdog stop: retry budget exhausted after 2 clean re-dispatches", false},
		{"non watchdog", "human review requested", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableStop(tc.reason); got != tc.want {
				t.Fatalf("IsRetryableStop(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}
