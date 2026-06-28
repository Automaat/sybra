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
func (left Usage) Add(right Usage) Usage {
	return Usage{
		CostUSD:                  left.CostUSD + right.CostUSD,
		InputTokens:              left.InputTokens + right.InputTokens,
		OutputTokens:             left.OutputTokens + right.OutputTokens,
		CacheCreationInputTokens: left.CacheCreationInputTokens + right.CacheCreationInputTokens,
		CacheReadInputTokens:     left.CacheReadInputTokens + right.CacheReadInputTokens,
		ReasoningTokens:          left.ReasoningTokens + right.ReasoningTokens,
		PremiumRequests:          left.PremiumRequests + right.PremiumRequests,
	}
}

// UsageAddPromotionBlocker prevents Agent from promoting Usage.Add as a
// mutating-looking method while keeping Usage embedded for flat JSON.
type UsageAddPromotionBlocker struct{}

func (UsageAddPromotionBlocker) Add(Usage) Usage {
	panic("UsageAddPromotionBlocker.Add is only embedded to keep Agent from promoting Usage.Add")
}
