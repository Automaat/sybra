package stats

import (
	"strings"
	"time"
)

const copilotAICreditUSD = 0.01

// Per-million-token USD rates. Cached input rates apply to the
// `cached_input_tokens` (Codex) or `cache_read_input_tokens` (Claude) subset.
// Cache-write rate applies to Claude `cache_creation_input_tokens`.
//
// Sources (OpenAI May 2026, Anthropic July 2026): platform.openai.com/docs/pricing,
// docs.claude.com/en/docs/about-claude/pricing.
type modelPrice struct {
	in           float64
	out          float64
	cacheRead    float64
	cacheWrite5m float64 // Claude only: 1.25× standard input
	cacheWrite1h float64 // Claude only: 2.00× standard input
}

type pricedTier struct {
	from  time.Time
	price modelPrice
}

var sonnet5StandardFrom = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

var (
	gpt5Price            = modelPrice{in: 1.25, out: 10.00, cacheRead: 0.125}
	gpt5MiniPrice        = modelPrice{in: 0.25, out: 2.00, cacheRead: 0.025}
	gpt5NanoPrice        = modelPrice{in: 0.05, out: 0.40, cacheRead: 0.005}
	haiku45Price         = modelPrice{in: 1.00, out: 5.00, cacheRead: 0.10, cacheWrite5m: 1.25, cacheWrite1h: 2.00}
	sonnet46Price        = modelPrice{in: 3.00, out: 15.00, cacheRead: 0.30, cacheWrite5m: 3.75, cacheWrite1h: 6.00}
	sonnet5IntroPrice    = modelPrice{in: 2.00, out: 10.00, cacheRead: 0.20, cacheWrite5m: 2.50, cacheWrite1h: 4.00}
	sonnet5StandardPrice = modelPrice{in: 3.00, out: 15.00, cacheRead: 0.30, cacheWrite5m: 3.75, cacheWrite1h: 6.00}
	opus48Price          = modelPrice{in: 5.00, out: 25.00, cacheRead: 0.50, cacheWrite5m: 6.25, cacheWrite1h: 10.00}
	opus41Price          = modelPrice{in: 15.00, out: 75.00, cacheRead: 1.50, cacheWrite5m: 18.75, cacheWrite1h: 30.00}
)

func tiers(price modelPrice) []pricedTier {
	return []pricedTier{{price: price}}
}

func sonnet5Tiers() []pricedTier {
	return []pricedTier{
		{price: sonnet5IntroPrice},
		{from: sonnet5StandardFrom, price: sonnet5StandardPrice},
	}
}

var pricingTable = map[string][]pricedTier{
	// OpenAI / Codex CLI defaults.
	// gpt-5 / gpt-5-mini are the underlying models behind `gpt-5.4` (codex
	// alias) and `gpt-5.4-mini`. Cached input is billed at 10% of standard.
	"gpt-5":         tiers(gpt5Price),
	"gpt-5-codex":   tiers(gpt5Price),
	"gpt-5-mini":    tiers(gpt5MiniPrice),
	"gpt-5-nano":    tiers(gpt5NanoPrice),
	"gpt-5.5":       tiers(gpt5Price), // codex default (0.142.2+)
	"gpt-5.4":       tiers(gpt5Price), // codex alias
	"gpt-5.4-mini":  tiers(gpt5MiniPrice),
	"gpt-5.3-codex": tiers(gpt5Price), // selectable codex option; gpt-5-codex pricing

	// Legacy OpenAI (kept for back-compat with old run records).
	"o4-mini":      tiers(modelPrice{in: 1.10, out: 4.40, cacheRead: 0.275}),
	"o3":           tiers(modelPrice{in: 2.00, out: 8.00, cacheRead: 0.50}),
	"o3-mini":      tiers(modelPrice{in: 1.10, out: 4.40, cacheRead: 0.55}),
	"gpt-4o":       tiers(modelPrice{in: 2.50, out: 10.00, cacheRead: 1.25}),
	"gpt-4o-mini":  tiers(modelPrice{in: 0.15, out: 0.60, cacheRead: 0.075}),
	"gpt-4.1":      tiers(modelPrice{in: 2.00, out: 8.00, cacheRead: 0.50}),
	"gpt-4.1-mini": tiers(modelPrice{in: 0.40, out: 1.60, cacheRead: 0.10}),
	"gpt-4.1-nano": tiers(modelPrice{in: 0.10, out: 0.40, cacheRead: 0.025}),

	// Anthropic — used for cross-checking Claude's reported total_cost_usd
	// and for runs where Claude's result event omits cost (older CLI versions).
	//
	// Pricing is effective-dated. To change a model rate, append a new tier
	// with its effective date; do not mutate an existing tier or historical
	// runs will be repriced when read-time estimates are recomputed.
	"claude-haiku-4-5":          tiers(haiku45Price),
	"claude-haiku-4-5-20251001": tiers(haiku45Price),
	"claude-sonnet-4-5":         tiers(sonnet46Price),
	"claude-sonnet-4-6":         tiers(sonnet46Price),
	"claude-sonnet-5":           sonnet5Tiers(),
	"claude-opus-4-1":           tiers(opus41Price),
	"claude-opus-4":             tiers(opus41Price),
	"claude-opus-4-5":           tiers(opus48Price),
	"claude-opus-4-6":           tiers(opus48Price),
	"claude-opus-4-7":           tiers(opus48Price),
	"claude-opus-4-8":           tiers(opus48Price),
	// Aliases used by `claude --model <name>` — map to current generation.
	"haiku":  tiers(haiku45Price),
	"sonnet": sonnet5Tiers(),
	"opus":   tiers(opus48Price),
}

// EstimateCost estimates USD cost from raw input/output tokens. Returns 0 for
// unknown models. Provided for back-compat with callers that don't track cache
// tokens — prefer EstimateCostDetailed when cache breakdown is available.
func EstimateCost(model string, inputTokens, outputTokens int, at time.Time) float64 {
	p, ok := lookupPrice(model, at)
	if !ok {
		return 0
	}
	return float64(inputTokens)/1_000_000*p.in + float64(outputTokens)/1_000_000*p.out
}

// EstimateCopilotCost converts Copilot AI credit usage to USD overage-equivalent cost.
func EstimateCopilotCost(premiumRequests float64) float64 {
	if premiumRequests <= 0 {
		return 0
	}
	return premiumRequests * copilotAICreditUSD
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
func EstimateCostDetailed(model string, input, output, cacheCreate, cacheRead, reasoning int, at time.Time) float64 {
	_ = reasoning // tokens already counted under `output`; arg kept for future per-tier pricing
	p, ok := lookupPrice(model, at)
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

func lookupPrice(model string, at time.Time) (modelPrice, bool) {
	if at.IsZero() {
		at = time.Now()
	}
	if tiers, ok := pricingTable[model]; ok {
		return priceForTier(tiers, at)
	}
	// Loose match: trim provider prefix (e.g. "openai/gpt-5" -> "gpt-5") and
	// drop trailing date suffixes ("-20251001"). Keeps the table small without
	// silently returning $0 for slightly-different model strings.
	stripped := stripModelSuffix(model)
	if tiers, ok := pricingTable[stripped]; ok {
		return priceForTier(tiers, at)
	}
	return modelPrice{}, false
}

func priceForTier(tiers []pricedTier, at time.Time) (modelPrice, bool) {
	if len(tiers) == 0 {
		return modelPrice{}, false
	}
	var selected modelPrice
	var selectedFrom time.Time
	found := false
	for _, tier := range tiers {
		if (tier.from.IsZero() || !tier.from.After(at)) && (!found || tier.from.After(selectedFrom)) {
			selected = tier.price
			selectedFrom = tier.from
			found = true
		}
	}
	return selected, found
}

func stripModelSuffix(m string) string {
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	// Trim trailing -YYYYMMDD if present.
	if len(m) > 9 && m[len(m)-9] == '-' {
		tail := m[len(m)-8:]
		allDigits := true
		for i := range len(tail) {
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
