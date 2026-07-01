package config

// ProvidersConfig groups per-machine routing for CLI providers (claude, codex,
// copilot) and their background health-check loop. A missing block defaults to
// "all providers enabled, health check on, auto-failover on, 300s interval".
type ProvidersConfig struct {
	HealthCheck  ProviderHealthCheckConfig `yaml:"health_check" json:"healthCheck"`
	Claude       ProviderEntryConfig       `yaml:"claude" json:"claude"`
	Codex        ProviderEntryConfig       `yaml:"codex" json:"codex"`
	Copilot      ProviderEntryConfig       `yaml:"copilot" json:"copilot"`
	Limits       ProviderLimitsConfig      `yaml:"limits" json:"limits"`
	AutoFailover bool                      `yaml:"auto_failover" json:"autoFailover"`
}

type ProviderHealthCheckConfig struct {
	Enabled         bool `yaml:"enabled" json:"enabled"`
	IntervalSeconds int  `yaml:"interval_seconds" json:"intervalSeconds"`
}

type ProviderEntryConfig struct {
	Enabled                  bool `yaml:"enabled" json:"enabled"`
	RateLimitCooldownSeconds int  `yaml:"rate_limit_cooldown_seconds" json:"rateLimitCooldownSeconds"`
	// MonthlySubscriptionUSD is optional and used only for Stats value
	// comparison. Zero means "not configured".
	MonthlySubscriptionUSD float64 `yaml:"monthly_subscription_usd" json:"monthlySubscriptionUsd"`
}

type ProviderLimitsConfig struct {
	Enabled                 bool    `yaml:"enabled" json:"enabled"`
	SessionThresholdPercent float64 `yaml:"session_threshold_percent" json:"sessionThresholdPercent"`
	WeeklyThresholdPercent  float64 `yaml:"weekly_threshold_percent" json:"weeklyThresholdPercent"`
	PreferUnderused         bool    `yaml:"prefer_underused" json:"preferUnderused"`
	BackfillDays            int     `yaml:"backfill_days" json:"backfillDays"`
	// MaxInFlightPerProvider caps concurrent in-flight agents per provider,
	// distinct from the global agent.max_concurrent ceiling. 0 (default)
	// disables the cap. When set, gateProvider redirects new dispatches away
	// from an at-cap provider even when PreferUnderused is false, so the cap
	// cannot silently no-op.
	MaxInFlightPerProvider int `yaml:"max_in_flight_per_provider" json:"maxInFlightPerProvider"`
}
