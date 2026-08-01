package watchdogreason

import "testing"

func TestIsRetryableStop(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   bool
	}{
		{"legacy bare stop", "watchdog stop", true},
		{"legacy natural loop stop", "watchdog: looping on toolchain setup", true},
		{"structured loop stop", "watchdog: loop stop: repeating command", true},
		{"budget stop", "watchdog: budget stop: repeating command", false},
		{"legacy budget stop", "watchdog: burned through budget with no forward progress", false},
		{"natural watchdog reason", "watchdog: repeating command", false},
		{"reward hacking kind", "watchdog: reward_hacking", false},
		{"reward hacking kind with detail", "watchdog: reward_hacking: repeated fake progress", false},
		{"reward hacking retry", "watchdog: reward-hacking retry: repeated fake progress", false},
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

func TestIsZeroOutputRateLimit(t *testing.T) {
	if !IsZeroOutputRateLimit(RateLimit(ZeroOutputBeforeStartup)) {
		t.Fatalf("expected zero-output watchdog rate limit to match")
	}
	if IsZeroOutputRateLimit(RateLimit("org-level quota exhausted")) {
		t.Fatalf("non-zero-output rate limit must not match")
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   Parsed
	}{
		{"hang", Hang("no stream activity"), Parsed{Kind: KindHang, Detail: "no stream activity"}},
		{"rate limit", RateLimit("quota exhausted"), Parsed{Kind: KindRateLimit, Detail: "quota exhausted"}},
		{"reward hacking retry", RewardHackingRetry("still looping"), Parsed{Kind: KindRewardHackingRetry, Detail: "still looping"}},
		{"verify failed", "watchdog: verify suite still fails after loop stop: go test ./...", Parsed{Kind: KindVerifyFailed, Detail: "go test ./..."}},
		{"unknown", "human review requested", Parsed{Kind: KindUnknown, Detail: "human review requested"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse(tc.reason); got != tc.want {
				t.Fatalf("Parse(%q) = %#v, want %#v", tc.reason, got, tc.want)
			}
		})
	}
}

func TestIsRewardHackingRetry(t *testing.T) {
	if !IsRewardHackingRetry(RewardHackingRetry("repeated fake progress")) {
		t.Fatal("expected reward-hacking retry reason to match")
	}
	if IsRewardHackingRetry(RewardHacking("repeated fake progress")) {
		t.Fatal("plain reward-hacking reason must not match retry classifier")
	}
}
