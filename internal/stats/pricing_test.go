package stats

import (
	"testing"
	"time"
)

func TestEstimateCost(t *testing.T) {
	beforeSonnetStandard := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	tests := []struct {
		model   string
		in, out int
		want    float64
	}{
		{"o4-mini", 1_000_000, 1_000_000, 1.10 + 4.40},
		{"o3", 1_000_000, 0, 2.00},
		{"gpt-4o", 0, 1_000_000, 10.00},
		{"gpt-4o-mini", 1_000_000, 1_000_000, 0.15 + 0.60},
		{"gpt-5", 1_000_000, 1_000_000, 1.25 + 10.00},
		{"gpt-5-mini", 1_000_000, 1_000_000, 0.25 + 2.00},
		{"gpt-5.5", 1_000_000, 1_000_000, 1.25 + 10.00},
		{"gpt-5.4", 1_000_000, 1_000_000, 1.25 + 10.00},
		{"gpt-5.4-mini", 1_000_000, 1_000_000, 0.25 + 2.00},
		{"gpt-5.3-codex", 1_000_000, 1_000_000, 1.25 + 10.00},
		{"sonnet", 1_000_000, 1_000_000, 2.00 + 10.00},
		{"haiku", 1_000_000, 1_000_000, 1.00 + 5.00},
		{"unknown-model", 1_000_000, 1_000_000, 0},
		{"", 100, 50, 0},
		{"o4-mini", 100, 50, 100.0/1_000_000*1.10 + 50.0/1_000_000*4.40},
	}
	for _, tt := range tests {
		got := EstimateCost(tt.model, tt.in, tt.out, beforeSonnetStandard)
		diff := got - tt.want
		if diff > 1e-9 || diff < -1e-9 {
			t.Errorf("EstimateCost(%q, %d, %d) = %g, want %g", tt.model, tt.in, tt.out, got, tt.want)
		}
	}
}

func TestEstimateCopilotCost(t *testing.T) {
	if got, want := EstimateCopilotCost(7.5), 0.075; got != want {
		t.Errorf("EstimateCopilotCost(7.5) = %g, want %g", got, want)
	}
	if got := EstimateCopilotCost(0); got != 0 {
		t.Errorf("EstimateCopilotCost(0) = %g, want 0", got)
	}
}

func TestEstimateCostDetailed_ClaudeMatchesReportedTotalCost(t *testing.T) {
	// Cross-check against a real Claude result event captured from a Sybra
	// agent run (logs/agents/ff4fb283-*.ndjson). Claude reports
	// total_cost_usd = 0.6614377500000002 for sonnet-4-6 with the breakdown
	// below — our estimator should land within $0.001.
	got := EstimateCostDetailed("claude-sonnet-4-6",
		24,      // usage.input_tokens
		10656,   // usage.output_tokens
		48071,   // usage.cache_creation_input_tokens (5m ephemeral)
		1070865, // usage.cache_read_input_tokens
		0,       // reasoning (Claude doesn't report)
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	)
	want := 0.6614377500000002
	diff := got - want
	if diff > 0.001 || diff < -0.001 {
		t.Errorf("EstimateCostDetailed sonnet-4-6 = %g, want %g (delta %g)", got, want, diff)
	}
}

func TestEstimateCostDetailed_CodexSubtractsCachedFromGrossInput(t *testing.T) {
	// Codex `usage.input_tokens` is the gross prompt total; `cached_input_tokens`
	// is a subset billed at the cache-read rate. The estimator must subtract
	// the cached subset before applying the standard input price.
	got := EstimateCostDetailed("gpt-5", 1_000_000, 100_000, 0, 800_000, 0, time.Time{})
	// Billed: 200k @ $1.25 + 100k @ $10 + 800k @ $0.125 = 0.25 + 1.00 + 0.10 = $1.35
	want := 1.35
	diff := got - want
	if diff > 1e-9 || diff < -1e-9 {
		t.Errorf("EstimateCostDetailed gpt-5 = %g, want %g", got, want)
	}
}

func TestLookupPrice_ClaudeEffectiveDatedPricing(t *testing.T) {
	intro := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	standard := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		model string
		at    time.Time
		want  modelPrice
		ok    bool
	}{
		{name: "sonnet 5 intro explicit", model: "claude-sonnet-5", at: intro, want: sonnet5IntroPrice, ok: true},
		{name: "sonnet alias intro", model: "sonnet", at: intro, want: sonnet5IntroPrice, ok: true},
		{name: "sonnet 5 standard explicit", model: "claude-sonnet-5", at: standard, want: sonnet5StandardPrice, ok: true},
		{name: "sonnet alias standard", model: "sonnet", at: standard, want: sonnet5StandardPrice, ok: true},
		{name: "sonnet dated suffix", model: "claude-sonnet-5-20260701", at: intro, want: sonnet5IntroPrice, ok: true},
		{name: "sonnet 4.6", model: "claude-sonnet-4-6", at: standard, want: sonnet46Price, ok: true},
		{name: "opus alias", model: "opus", at: standard, want: opus48Price, ok: true},
		{name: "opus 4.5", model: "claude-opus-4-5", at: standard, want: opus48Price, ok: true},
		{name: "opus 4.6", model: "claude-opus-4-6", at: standard, want: opus48Price, ok: true},
		{name: "opus 4.7", model: "claude-opus-4-7", at: standard, want: opus48Price, ok: true},
		{name: "opus 4.8", model: "claude-opus-4-8", at: standard, want: opus48Price, ok: true},
		{name: "opus 4.1 stays legacy", model: "claude-opus-4-1", at: standard, want: opus41Price, ok: true},
		{name: "unknown", model: "unknown-model", at: standard, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lookupPrice(tt.model, tt.at)
			if ok != tt.ok {
				t.Fatalf("lookupPrice(%q) ok = %v, want %v", tt.model, ok, tt.ok)
			}
			if !ok {
				return
			}
			if got != tt.want {
				t.Fatalf("lookupPrice(%q) = %#v, want %#v", tt.model, got, tt.want)
			}
		})
	}
}

func TestPriceForTier_SelectsLatestEligibleTierOutOfOrder(t *testing.T) {
	t.Parallel()

	intro := modelPrice{in: 2, out: 10}
	standard := modelPrice{in: 3, out: 15}
	future := modelPrice{in: 4, out: 20}
	standardFrom := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	futureFrom := standardFrom.Add(24 * time.Hour)

	got, ok := priceForTier([]pricedTier{
		{from: futureFrom, price: future},
		{from: standardFrom, price: standard},
		{price: intro},
	}, standardFrom)
	if !ok {
		t.Fatal("priceForTier returned ok=false")
	}
	if got != standard {
		t.Fatalf("priceForTier = %#v, want %#v", got, standard)
	}
}

func TestLookupPrice_ZeroTimeFallsBackToNow(t *testing.T) {
	if _, ok := lookupPrice("gpt-5", time.Time{}); !ok {
		t.Fatal("lookupPrice with zero time should still resolve known models")
	}
}

func TestStripModelSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"openai/gpt-5", "gpt-5"},
		{"gpt-5", "gpt-5"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripModelSuffix(c.in); got != c.want {
			t.Errorf("stripModelSuffix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
