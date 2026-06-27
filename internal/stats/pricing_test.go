package stats

import "testing"

func TestEstimateCost(t *testing.T) {
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
		{"sonnet", 1_000_000, 1_000_000, 3.00 + 15.00},
		{"haiku", 1_000_000, 1_000_000, 1.00 + 5.00},
		{"unknown-model", 1_000_000, 1_000_000, 0},
		{"", 100, 50, 0},
		{"o4-mini", 100, 50, 100.0/1_000_000*1.10 + 50.0/1_000_000*4.40},
	}
	for _, tt := range tests {
		got := EstimateCost(tt.model, tt.in, tt.out)
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
	got := EstimateCostDetailed("gpt-5", 1_000_000, 100_000, 0, 800_000, 0)
	// Billed: 200k @ $1.25 + 100k @ $10 + 800k @ $0.125 = 0.25 + 1.00 + 0.10 = $1.35
	want := 1.35
	diff := got - want
	if diff > 1e-9 || diff < -1e-9 {
		t.Errorf("EstimateCostDetailed gpt-5 = %g, want %g", got, want)
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
