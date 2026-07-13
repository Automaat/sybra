package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/provider"
)

func TestProviderHealthMetricsTreatsFailoverAsAlertHealthy(t *testing.T) {
	snapshot := map[string]provider.Status{
		"claude": {Provider: "claude", Healthy: false, Reason: provider.RateLimitReason},
		"codex":  {Provider: "codex", Healthy: true, Reason: "ok"},
	}
	failover := func(name string) string {
		if name == "claude" {
			return "codex"
		}
		return ""
	}

	alertHealth, rawHealth := providerHealthMetrics(snapshot, failover)

	if got := alertHealth["claude"]; got != 1 {
		t.Fatalf("alert health for failover-covered claude = %d, want 1", got)
	}
	if got := rawHealth["claude"]; got != 0 {
		t.Fatalf("raw health for rate-limited claude = %d, want 0", got)
	}
	if got := alertHealth["codex"]; got != 1 {
		t.Fatalf("alert health for healthy codex = %d, want 1", got)
	}
	if got := rawHealth["codex"]; got != 1 {
		t.Fatalf("raw health for healthy codex = %d, want 1", got)
	}
}

func TestProviderHealthMetricsAlertsWhenNoFailoverPath(t *testing.T) {
	snapshot := map[string]provider.Status{
		"claude": {Provider: "claude", Healthy: false, Reason: provider.RateLimitReason},
		"codex":  {Provider: "codex", Healthy: false, Reason: "logged_out"},
	}

	alertHealth, rawHealth := providerHealthMetrics(snapshot, func(string) string { return "" })

	if got := alertHealth["claude"]; got != 0 {
		t.Fatalf("alert health for unavailable claude = %d, want 0", got)
	}
	if got := rawHealth["claude"]; got != 0 {
		t.Fatalf("raw health for unavailable claude = %d, want 0", got)
	}
	if got := alertHealth["codex"]; got != 0 {
		t.Fatalf("alert health for unavailable codex = %d, want 0", got)
	}
	if got := rawHealth["codex"]; got != 0 {
		t.Fatalf("raw health for unavailable codex = %d, want 0", got)
	}
}
