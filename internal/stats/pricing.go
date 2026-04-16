package stats

// EstimateCost estimates USD cost from token counts using a static pricing table.
// Returns 0 if the model is not in the table.
func EstimateCost(model string, inputTokens, outputTokens int) float64 {
	type price struct{ in, out float64 } // USD per 1M tokens
	table := map[string]price{
		"o4-mini":      {1.10, 4.40},
		"o3":           {10.00, 40.00},
		"o3-mini":      {1.10, 4.40},
		"gpt-4o":       {2.50, 10.00},
		"gpt-4o-mini":  {0.15, 0.60},
		"gpt-4.1":      {2.00, 8.00},
		"gpt-4.1-mini": {0.40, 1.60},
		"gpt-4.1-nano": {0.10, 0.40},
	}
	p, ok := table[model]
	if !ok {
		return 0
	}
	return float64(inputTokens)/1_000_000*p.in + float64(outputTokens)/1_000_000*p.out
}
