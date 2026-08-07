package modeltier

import (
	"maps"
	"strings"

	"github.com/Automaat/sybra/internal/providerid"
)

// Tier names Sybra's provider-neutral model cost/capability classes.
type Tier string

const (
	// SuperCheap is the haiku/mini class for small structured jobs.
	SuperCheap Tier = "super_cheap"
	// Cheap is the sonnet-class default for ordinary code-authoring work.
	Cheap Tier = "cheap"
	// Expensive is the opus/deep class for high-scrutiny planning/review.
	Expensive Tier = "expensive"
)

var models = map[Tier]map[string]string{
	SuperCheap: {
		providerid.Claude:   "haiku",
		providerid.Codex:    "gpt-5.6-luna",
		providerid.Copilot:  "gpt-5-mini",
		providerid.OpenCode: "openrouter/qwen/qwen3-32b",
	},
	Cheap: {
		providerid.Claude:   "sonnet",
		providerid.Codex:    "gpt-5.6-terra",
		providerid.Copilot:  "claude-sonnet-4.6",
		providerid.OpenCode: "openrouter/deepseek/deepseek-v4-flash",
	},
	Expensive: {
		providerid.Claude:   "opus",
		providerid.Codex:    "gpt-5.6-sol",
		providerid.Copilot:  "gemini-3.1-pro-preview",
		providerid.OpenCode: "openrouter/z-ai/glm-5.2",
	},
}

var tierAliases = map[Tier]string{
	SuperCheap: "haiku",
	Cheap:      "sonnet",
	Expensive:  "opus",
}

var aliasToTier = map[string]Tier{
	"":            Cheap,
	"cheap":       Cheap,
	"expensive":   Expensive,
	"haiku":       SuperCheap,
	"opus":        Expensive,
	"sonnet":      Cheap,
	"super_cheap": SuperCheap,
	"supercheap":  SuperCheap,
}

// Models returns a defensive copy of the provider model map for tier.
func Models(tier Tier) map[string]string {
	row, ok := models[tier]
	if !ok {
		row = models[Cheap]
	}
	out := make(map[string]string, len(row))
	maps.Copy(out, row)
	return out
}

// Model returns the provider-specific model for tier, or an empty string when
// the provider has no mapping.
func Model(tier Tier, provider string) string {
	return models[tier][provider]
}

// Alias returns the provider-neutral alias for tier.
func Alias(tier Tier) string {
	return tierAliases[tier]
}

// InferTier maps a provider-neutral alias or one of Sybra's known concrete
// provider model IDs back to its neutral capability tier.
func InferTier(model string) (Tier, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(model))
	if tier, ok := aliasToTier[trimmed]; ok {
		return tier, true
	}
	if trimmed == "gpt-5.6" {
		// Codex's bare generation alias resolves to Sol. Matched exactly here
		// rather than by Contains below, where it would also swallow the
		// -terra and -luna slugs and misclassify them as Expensive.
		return Expensive, true
	}
	for tier, row := range models {
		for _, candidate := range row {
			if strings.EqualFold(trimmed, candidate) {
				return tier, true
			}
		}
	}
	// The retired gpt-5.4/5.5 slugs stay matched so historical run records and
	// hand-pinned task YAML still classify into a tier.
	switch {
	case strings.Contains(trimmed, "haiku"),
		strings.Contains(trimmed, "gpt-5.6-luna"),
		strings.Contains(trimmed, "gpt-5.4-mini"),
		strings.Contains(trimmed, "qwen3-32b"):
		return SuperCheap, true
	case strings.Contains(trimmed, "opus"),
		strings.Contains(trimmed, "gpt-5.6-sol"),
		strings.Contains(trimmed, "gpt-5.5"),
		strings.Contains(trimmed, "gemini-3.1-pro"),
		strings.Contains(trimmed, "glm-5.2"):
		return Expensive, true
	case strings.Contains(trimmed, "sonnet"),
		strings.Contains(trimmed, "gpt-5.6-terra"),
		strings.Contains(trimmed, "gpt-5.4"),
		strings.Contains(trimmed, "deepseek-v4-flash"):
		return Cheap, true
	default:
		return "", false
	}
}

// NormalizeAlias maps Sybra's provider-neutral model aliases to a concrete
// provider model. The boolean is false when model is already provider-specific
// and should pass through unchanged.
func NormalizeAlias(provider, model string) (string, bool) {
	tier, ok := aliasToTier[strings.ToLower(strings.TrimSpace(model))]
	if !ok {
		return model, false
	}
	resolved := Model(tier, provider)
	if strings.TrimSpace(resolved) == "" {
		return model, false
	}
	return resolved, true
}
