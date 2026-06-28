package limits

import "time"

const (
	ProviderClaude  = "claude"
	ProviderCodex   = "codex"
	ProviderCopilot = "copilot"

	SourceStream       = "stream"
	SourceSessionFiles = "session-files"
	SourceRunStats     = "run-stats"
	SourceLivePoll     = "live-poll"

	ConfidenceExact     = "exact"
	ConfidenceEstimated = "estimated"
)

// CycleSnapshot captures a provider-reported rolling limit window.
type CycleSnapshot struct {
	UsedPercent   float64   `json:"usedPercent"`
	WindowMinutes int       `json:"windowMinutes"`
	ResetsAt      time.Time `json:"resetsAt,omitzero"`
}

// Snapshot is the latest known quota/limit state for one provider.
type Snapshot struct {
	Provider             string         `json:"provider"`
	PlanType             string         `json:"planType,omitempty"`
	LimitID              string         `json:"limitId,omitempty"`
	LimitName            string         `json:"limitName,omitempty"`
	RateLimitReachedType string         `json:"rateLimitReachedType,omitempty"`
	Primary              *CycleSnapshot `json:"primary,omitempty"`
	Secondary            *CycleSnapshot `json:"secondary,omitempty"`
	Source               string         `json:"source"`
	Confidence           string         `json:"confidence"`
	CapturedAt           time.Time      `json:"capturedAt"`
}

// UsageEvent records local provider usage from Sybra runs or provider session
// files. It deliberately stores only counters and IDs, never prompt/content.
type UsageEvent struct {
	ID                       string    `json:"id"`
	Provider                 string    `json:"provider"`
	Source                   string    `json:"source"`
	TaskID                   string    `json:"taskId,omitempty"`
	AgentID                  string    `json:"agentId,omitempty"`
	SessionID                string    `json:"sessionId,omitempty"`
	Model                    string    `json:"model,omitempty"`
	CostUSD                  float64   `json:"costUsd,omitempty"`
	InputTokens              int       `json:"inputTokens,omitempty"`
	OutputTokens             int       `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int       `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int       `json:"cacheReadInputTokens,omitempty"`
	ReasoningTokens          int       `json:"reasoningTokens,omitempty"`
	TotalTokens              int       `json:"totalTokens,omitempty"`
	PremiumRequests          float64   `json:"premiumRequests,omitempty"`
	Timestamp                time.Time `json:"timestamp"`
}

// ProviderSummary is the frontend/statistics view of one provider's limit and
// API-equivalent spend. Exact quota percentages are included only when a
// provider exposes them locally.
type ProviderSummary struct {
	Provider                    string    `json:"provider"`
	PlanType                    string    `json:"planType,omitempty"`
	LimitID                     string    `json:"limitId,omitempty"`
	Source                      string    `json:"source,omitempty"`
	Confidence                  string    `json:"confidence,omitempty"`
	SessionUsedPercent          float64   `json:"sessionUsedPercent,omitempty"`
	SessionResetsAt             time.Time `json:"sessionResetsAt,omitzero"`
	SessionWindowMinutes        int       `json:"sessionWindowMinutes,omitempty"`
	WeeklyUsedPercent           float64   `json:"weeklyUsedPercent,omitempty"`
	WeeklyResetsAt              time.Time `json:"weeklyResetsAt,omitzero"`
	WeeklyWindowMinutes         int       `json:"weeklyWindowMinutes,omitempty"`
	SessionSpendUSD             float64   `json:"sessionSpendUsd,omitempty"`
	WeeklySpendUSD              float64   `json:"weeklySpendUsd,omitempty"`
	SessionInputTokens          int       `json:"sessionInputTokens,omitempty"`
	SessionOutputTokens         int       `json:"sessionOutputTokens,omitempty"`
	SessionCacheReadTokens      int       `json:"sessionCacheReadTokens,omitempty"`
	SessionReasoningTokens      int       `json:"sessionReasoningTokens,omitempty"`
	SessionPremiumRequests      float64   `json:"sessionPremiumRequests,omitempty"`
	WeeklyInputTokens           int       `json:"weeklyInputTokens,omitempty"`
	WeeklyOutputTokens          int       `json:"weeklyOutputTokens,omitempty"`
	WeeklyCacheReadTokens       int       `json:"weeklyCacheReadTokens,omitempty"`
	WeeklyReasoningTokens       int       `json:"weeklyReasoningTokens,omitempty"`
	WeeklyPremiumRequests       float64   `json:"weeklyPremiumRequests,omitempty"`
	MonthlySubscriptionUSD      float64   `json:"monthlySubscriptionUsd,omitempty"`
	MonthlySubscriptionBurnRate float64   `json:"monthlySubscriptionBurnRate,omitempty"`
	QuotaLimited                bool      `json:"quotaLimited,omitempty"`
	QuotaReason                 string    `json:"quotaReason,omitempty"`
}

// Summary is embedded in stats.StatsResponse.
type Summary struct {
	Providers []ProviderSummary `json:"providers"`
	UpdatedAt time.Time         `json:"updatedAt,omitzero"`
}

// Policy controls provider selection and stats annotations.
type Policy struct {
	Enabled                 bool
	SessionThresholdPercent float64
	WeeklyThresholdPercent  float64
	PreferUnderused         bool
	SubscriptionMonthlyUSD  map[string]float64
	ProviderEnabled         map[string]bool
}

func DefaultPolicy() Policy {
	return Policy{
		Enabled:                 true,
		SessionThresholdPercent: 85,
		WeeklyThresholdPercent:  90,
		PreferUnderused:         true,
		SubscriptionMonthlyUSD:  map[string]float64{},
		ProviderEnabled: map[string]bool{
			ProviderClaude:  true,
			ProviderCodex:   true,
			ProviderCopilot: true,
		},
	}
}
