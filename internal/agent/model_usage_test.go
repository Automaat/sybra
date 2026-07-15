package agent

import "testing"

func TestAgentCumulativeUsageUpdates(t *testing.T) {
	a := &Agent{}

	if got := a.AddResultStats("session-a", 1.25, 10, 20, 3); got != 1.25 {
		t.Fatalf("first cumulative cost = %v, want 1.25", got)
	}
	a.AddCacheStats(5, 7)
	a.AddPremiumRequests(0.5)
	a.AddOutputTokens(4)
	// Second event carries no session id — it continues session-a, whose
	// reported cost (like every provider's total_cost_usd) is already a
	// cumulative total for that session, so it replaces rather than adds to
	// the first snapshot.
	if got := a.AddResultStats("", 2.75, 30, 40, 6); got != 2.75 {
		t.Fatalf("second cumulative cost = %v, want 2.75", got)
	}
	a.AddCacheStats(11, 13)
	a.AddPremiumRequests(1.25)
	a.AddOutputTokens(8)

	if a.SessionID != "session-a" {
		t.Fatalf("SessionID = %q, want session-a", a.SessionID)
	}
	if a.CostUSD != 2.75 {
		t.Fatalf("CostUSD = %v, want 2.75", a.CostUSD)
	}
	if a.InputTokens != 40 {
		t.Fatalf("InputTokens = %d, want 40", a.InputTokens)
	}
	if a.OutputTokens != 72 {
		t.Fatalf("OutputTokens = %d, want 72", a.OutputTokens)
	}
	if a.CacheCreationInputTokens != 16 {
		t.Fatalf("CacheCreationInputTokens = %d, want 16", a.CacheCreationInputTokens)
	}
	if a.CacheReadInputTokens != 20 {
		t.Fatalf("CacheReadInputTokens = %d, want 20", a.CacheReadInputTokens)
	}
	if a.ReasoningTokens != 9 {
		t.Fatalf("ReasoningTokens = %d, want 9", a.ReasoningTokens)
	}
	if a.PremiumRequests != 1.75 {
		t.Fatalf("PremiumRequests = %v, want 1.75", a.PremiumRequests)
	}
}

// TestAgentCostUSD_SameSessionReplacesNotAdds is the regression guard for
// the multi-segment cost inflation bug: a --resume'd provider process keeps
// reporting the same session id, and its cost is a cumulative total for that
// whole session (not a per-turn delta) — so successive results within one
// session must replace the running cost, never sum on top of it.
func TestAgentCostUSD_SameSessionReplacesNotAdds(t *testing.T) {
	a := &Agent{}

	a.AddResultStats("sess-1", 5.0, 0, 0, 0)
	if got := a.AddResultStats("sess-1", 6.0, 0, 0, 0); got != 6.0 {
		t.Fatalf("cost after second same-session result = %v, want 6.0 (replace, not 11.0)", got)
	}
	if a.CostUSD != 6.0 {
		t.Fatalf("CostUSD = %v, want 6.0", a.CostUSD)
	}
}

// TestAgentCostUSD_NewSessionBanksPriorTotal verifies that a genuinely new
// session (e.g. a later resume segment that gets its own session id) adds on
// top of the prior session's final cumulative snapshot instead of replacing
// it outright — each session's own total is real spend that must be kept.
func TestAgentCostUSD_NewSessionBanksPriorTotal(t *testing.T) {
	a := &Agent{}

	a.AddResultStats("sess-1", 0.10, 0, 0, 0)
	if got := a.AddResultStats("sess-2", 0.20, 0, 0, 0); got < 0.29 || got > 0.31 {
		t.Fatalf("cost after new-session result = %v, want ~0.30", got)
	}
}
