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
	if got := a.AddResultStats("", 2.75, 30, 40, 6); got != 4 {
		t.Fatalf("second cumulative cost = %v, want 4", got)
	}
	a.AddCacheStats(11, 13)
	a.AddPremiumRequests(1.25)
	a.AddOutputTokens(8)

	if a.SessionID != "session-a" {
		t.Fatalf("SessionID = %q, want session-a", a.SessionID)
	}
	if a.CostUSD != 4 {
		t.Fatalf("CostUSD = %v, want 4", a.CostUSD)
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
