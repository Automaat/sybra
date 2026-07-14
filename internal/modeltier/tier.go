package modeltier

import (
	"maps"
	"strings"
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
		"claude":   "haiku",
		"codex":    "gpt-5.4-mini",
		"copilot":  "gpt-5-mini",
		"opencode": "openrouter/qwen/qwen3-32b",
	},
	Cheap: {
		"claude":   "sonnet",
		"codex":    "gpt-5.4",
		"copilot":  "claude-sonnet-4.6",
		"opencode": "openrouter/deepseek/deepseek-v4-flash",
	},
	Expensive: {
		"claude":   "opus",
		"codex":    "gpt-5.5",
		"copilot":  "gemini-3.1-pro-preview",
		"opencode": "openrouter/z-ai/glm-5.2",
	},
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

// NormalizeAlias maps Sybra's provider-neutral model aliases to a concrete
// provider model. The boolean is false when model is already provider-specific
// and should pass through unchanged.
func NormalizeAlias(provider, model string) (string, bool) {
	switch strings.TrimSpace(model) {
	case "", "sonnet":
		return Model(Cheap, provider), true
	case "haiku":
		return Model(SuperCheap, provider), true
	case "opus":
		return Model(Expensive, provider), true
	default:
		return model, false
	}
}
