package modeltier

import (
	"maps"
	"regexp"
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

var tierAliases = map[Tier]string{
	SuperCheap: "haiku",
	Cheap:      "sonnet",
	Expensive:  "opus",
}

var claudeVersionedModelRe = regexp.MustCompile(`^claude-(haiku|sonnet|opus)-[0-9]+(?:[.-][0-9]+)*(?:-[0-9]{8})?$`)

var concreteModelTiers = map[string]Tier{
	"gemini-3.1-pro":         Expensive,
	"gemini-3.1-pro-preview": Expensive,
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
	switch trimmed {
	case "", "sonnet":
		return Cheap, true
	case "haiku":
		return SuperCheap, true
	case "opus":
		return Expensive, true
	}
	for tier, row := range models {
		for _, candidate := range row {
			if strings.EqualFold(trimmed, candidate) {
				return tier, true
			}
		}
	}
	if tier, ok := concreteModelTiers[trimmed]; ok {
		return tier, true
	}
	if match := claudeVersionedModelRe.FindStringSubmatch(trimmed); len(match) == 2 {
		switch match[1] {
		case "haiku":
			return SuperCheap, true
		case "sonnet":
			return Cheap, true
		case "opus":
			return Expensive, true
		}
	}
	return "", false
}

// NormalizeAlias maps Sybra's provider-neutral model aliases to a concrete
// provider model. The boolean is false when model is already provider-specific
// and should pass through unchanged.
func NormalizeAlias(provider, model string) (string, bool) {
	var tier Tier
	switch strings.TrimSpace(model) {
	case "", "sonnet":
		tier = Cheap
	case "haiku":
		tier = SuperCheap
	case "opus":
		tier = Expensive
	default:
		return model, false
	}
	resolved := Model(tier, provider)
	if strings.TrimSpace(resolved) == "" {
		return model, false
	}
	return resolved, true
}
