package agent

// Usage captures the cost and token consumption of one provider interaction.
type Usage struct {
	CostUSD                  float64 `json:"costUsd"`
	InputTokens              int     `json:"inputTokens,omitempty"`
	OutputTokens             int     `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	ReasoningTokens          int     `json:"reasoningTokens,omitempty"`
	// PremiumRequests is Copilot's billing unit (AI credits). Sybra keeps the
	// raw count alongside the estimated USD equivalent persisted on task runs.
	PremiumRequests float64 `json:"premiumRequests,omitempty"`
}

// Add returns the sum of two usage values.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		CostUSD:                  u.CostUSD + other.CostUSD,
		InputTokens:              u.InputTokens + other.InputTokens,
		OutputTokens:             u.OutputTokens + other.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens + other.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens + other.CacheReadInputTokens,
		ReasoningTokens:          u.ReasoningTokens + other.ReasoningTokens,
		PremiumRequests:          u.PremiumRequests + other.PremiumRequests,
	}
}
