package llmjob

import "maps"

// Tier describes the cost/capability class for a short structured LLM job.
type Tier int

const (
	Cheap Tier = iota
	Standard
	Deep
)

var tierModels = map[Tier]map[string]string{
	Cheap: {
		"claude": "haiku",
		"codex":  "gpt-5.4-mini",
		// GPT-5 mini and GPT-4.1 are Copilot's no-premium-request tiers.
		"copilot": "gpt-5-mini",
	},
	Standard: {
		"claude":  "sonnet",
		"codex":   "gpt-5.5",
		"copilot": "claude-sonnet-4.6",
	},
	Deep: {
		"claude":  "opus",
		"codex":   "gpt-5.5",
		"copilot": "gemini-3.1-pro-preview",
	},
}

func modelsFor(t Tier) map[string]string {
	row, ok := tierModels[t]
	if !ok {
		row = tierModels[Standard]
	}
	out := make(map[string]string, len(row))
	maps.Copy(out, row)
	return out
}
