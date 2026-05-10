package stats

import "strings"

// Per-million-token USD rates. Cached input rates apply to the
// `cached_input_tokens` (Codex) or `cache_read_input_tokens` (Claude) subset.
// Cache-write rate applies to Claude `cache_creation_input_tokens`.
//
// Sources (May 2026): platform.openai.com/docs/pricing,
// docs.claude.com/en/docs/about-claude/pricing.
type modelPrice struct {
	in           float64
	out          float64
	cacheRead    float64
	cacheWrite5m float64 // Claude only: 1.25× standard input
	cacheWrite1h float64 // Claude only: 2.00× standard input
}

var pricingTable = map[string]modelPrice{
	// OpenAI / Codex CLI defaults.
	// gpt-5 / gpt-5-mini are the underlying models behind `gpt-5.4` (codex
	// alias) and `gpt-5.4-mini`. Cached input is billed at 10% of standard.
	"gpt-5":        {in: 1.25, out: 10.00, cacheRead: 0.125},
	"gpt-5-codex":  {in: 1.25, out: 10.00, cacheRead: 0.125},
	"gpt-5-mini":   {in: 0.25, out: 2.00, cacheRead: 0.025},
	"gpt-5-nano":   {in: 0.05, out: 0.40, cacheRead: 0.005},
	"gpt-5.4":      {in: 1.25, out: 10.00, cacheRead: 0.125}, // codex alias
	"gpt-5.4-mini": {in: 0.25, out: 2.00, cacheRead: 0.025},  // codex alias

	// Legacy OpenAI (kept for back-compat with old run records).
	"o4-mini":      {in: 1.10, out: 4.40, cacheRead: 0.275},
	"o3":           {in: 2.00, out: 8.00, cacheRead: 0.50},
	"o3-mini":      {in: 1.10, out: 4.40, cacheRead: 0.55},
	"gpt-4o":       {in: 2.50, out: 10.00, cacheRead: 1.25},
	"gpt-4o-mini":  {in: 0.15, out: 0.60, cacheRead: 0.075},
	"gpt-4.1":      {in: 2.00, out: 8.00, cacheRead: 0.50},
	"gpt-4.1-mini": {in: 0.40, out: 1.60, cacheRead: 0.10},
	"gpt-4.1-nano": {in: 0.10, out: 0.40, cacheRead: 0.025},

	// Anthropic — used for cross-checking Claude's reported total_cost_usd
	// and for runs where Claude's result event omits cost (older CLI versions).
	"claude-haiku-4-5":          {in: 1.00, out: 5.00, cacheRead: 0.10, cacheWrite5m: 1.25, cacheWrite1h: 2.00},
	"claude-haiku-4-5-20251001": {in: 1.00, out: 5.00, cacheRead: 0.10, cacheWrite5m: 1.25, cacheWrite1h: 2.00},
	"claude-sonnet-4-5":         {in: 3.00, out: 15.00, cacheRead: 0.30, cacheWrite5m: 3.75, cacheWrite1h: 6.00},
	"claude-sonnet-4-6":         {in: 3.00, out: 15.00, cacheRead: 0.30, cacheWrite5m: 3.75, cacheWrite1h: 6.00},
	"claude-opus-4-5":           {in: 15.00, out: 75.00, cacheRead: 1.50, cacheWrite5m: 18.75, cacheWrite1h: 30.00},
	"claude-opus-4-6":           {in: 15.00, out: 75.00, cacheRead: 1.50, cacheWrite5m: 18.75, cacheWrite1h: 30.00},
	"claude-opus-4-7":           {in: 15.00, out: 75.00, cacheRead: 1.50, cacheWrite5m: 18.75, cacheWrite1h: 30.00},
	// Aliases used by `claude --model <name>` — map to current generation.
	"haiku":  {in: 1.00, out: 5.00, cacheRead: 0.10, cacheWrite5m: 1.25, cacheWrite1h: 2.00},
	"sonnet": {in: 3.00, out: 15.00, cacheRead: 0.30, cacheWrite5m: 3.75, cacheWrite1h: 6.00},
	"opus":   {in: 15.00, out: 75.00, cacheRead: 1.50, cacheWrite5m: 18.75, cacheWrite1h: 30.00},
}

// EstimateCost estimates USD cost from raw input/output tokens. Returns 0 for
// unknown models. Provided for back-compat with callers that don't track cache
// tokens — prefer EstimateCostDetailed when cache breakdown is available.
func EstimateCost(model string, inputTokens, outputTokens int) float64 {
	p, ok := lookupPrice(model)
	if !ok {
		return 0
	}
	return float64(inputTokens)/1_000_000*p.in + float64(outputTokens)/1_000_000*p.out
}

// EstimateCostDetailed prices a run using the full token breakdown.
//
// For Codex: input is the gross `input_tokens` (which already includes the
// cached subset), so we subtract cacheRead before pricing at the standard rate.
// cacheCreate should be 0 (Codex has no cache-write concept).
//
// For Claude: input is `usage.input_tokens` (uncached only — distinct from
// cache_creation/read), cacheCreate is `usage.cache_creation_input_tokens`,
// cacheRead is `usage.cache_read_input_tokens`. Reasoning output tokens are
// already part of `output_tokens` for both providers, so the `reasoning` arg
// is informational and not double-billed here.
func EstimateCostDetailed(model string, input, output, cacheCreate, cacheRead, reasoning int) float64 {
	_ = reasoning // tokens already counted under `output`; arg kept for future per-tier pricing
	p, ok := lookupPrice(model)
	if !ok {
		return 0
	}
	billedInput := input
	if isCodexModel(model) && cacheRead > 0 && billedInput >= cacheRead {
		// Codex `input_tokens` is gross; cached subset is billed separately.
		billedInput -= cacheRead
	}
	cacheWriteRate := p.cacheWrite5m
	if cacheWriteRate == 0 {
		cacheWriteRate = p.in // fallback for non-Claude providers
	}
	return float64(billedInput)/1_000_000*p.in +
		float64(output)/1_000_000*p.out +
		float64(cacheRead)/1_000_000*p.cacheRead +
		float64(cacheCreate)/1_000_000*cacheWriteRate
}

func lookupPrice(model string) (modelPrice, bool) {
	p, ok := pricingTable[model]
	if ok {
		return p, true
	}
	// Loose match: trim provider prefix (e.g. "openai/gpt-5" -> "gpt-5") and
	// drop trailing date suffixes ("-20251001"). Keeps the table small without
	// silently returning $0 for slightly-different model strings.
	stripped := stripModelSuffix(model)
	if p, ok := pricingTable[stripped]; ok {
		return p, true
	}
	return modelPrice{}, false
}

func stripModelSuffix(m string) string {
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	// Trim trailing -YYYYMMDD if present.
	if len(m) > 9 && m[len(m)-9] == '-' {
		tail := m[len(m)-8:]
		allDigits := true
		for i := 0; i < len(tail); i++ {
			if tail[i] < '0' || tail[i] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return m[:len(m)-9]
		}
	}
	return m
}

func isCodexModel(model string) bool {
	stripped := stripModelSuffix(model)
	return strings.HasPrefix(stripped, "gpt-") || strings.HasPrefix(stripped, "o3") || strings.HasPrefix(stripped, "o4")
}
