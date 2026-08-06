package config

import "github.com/Automaat/sybra/internal/providerid"

// ProvidersConfig groups per-machine routing for CLI providers (claude, codex,
// copilot, opencode) and their background health-check loop. A missing block defaults to
// "all providers enabled, health check on, auto-failover on, 300s interval".
type ProvidersConfig struct {
	HealthCheck  ProviderHealthCheckConfig `yaml:"health_check" json:"healthCheck"`
	Claude       ProviderEntryConfig       `yaml:"claude" json:"claude"`
	Codex        ProviderEntryConfig       `yaml:"codex" json:"codex"`
	Copilot      ProviderEntryConfig       `yaml:"copilot" json:"copilot"`
	OpenCode     ProviderEntryConfig       `yaml:"opencode" json:"opencode"`
	Limits       ProviderLimitsConfig      `yaml:"limits" json:"limits"`
	AutoFailover bool                      `yaml:"auto_failover" json:"autoFailover"`
}

// EnabledNames returns the enabled providers in failover-preference order.
// Callers that report capacity need the same ordered list the dispatcher walks,
// so a one-leg chain is recognisable as one. The universe and its order come
// from providerid.All rather than a second local list, which would drift the
// first time a provider is added or reordered.
func (p ProvidersConfig) EnabledNames() []string {
	universe := providerid.All()
	out := make([]string, 0, len(universe))
	for _, name := range universe {
		if entry, ok := p.entryFor(name); ok && entry.Enabled {
			out = append(out, name)
		}
	}
	return out
}

// entryFor maps a provider id onto its config block. The false return is the
// drift alarm: a provider added to providerid.All without a block here is
// silently absent from every capacity report otherwise.
func (p ProvidersConfig) entryFor(name string) (ProviderEntryConfig, bool) {
	switch name {
	case providerid.Claude:
		return p.Claude, true
	case providerid.Codex:
		return p.Codex, true
	case providerid.Copilot:
		return p.Copilot, true
	case providerid.OpenCode:
		return p.OpenCode, true
	default:
		return ProviderEntryConfig{}, false
	}
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

type RoutingEligibleVariant struct {
	ExperimentID string `json:"experimentId"`
	VariantID    string `json:"variantId"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Eligible     bool   `json:"eligible"`
	Reason       string `json:"reason,omitempty"`
}

type RoutingSummary struct {
	ProviderPreference     string                   `json:"providerPreference"`
	ABTestingEnabled       bool                     `json:"abTestingEnabled"`
	ABTestingExplicit      bool                     `json:"abTestingExplicit"`
	AdaptiveRoutingEnabled bool                     `json:"adaptiveRoutingEnabled"`
	AutoFailoverEnabled    bool                     `json:"autoFailoverEnabled"`
	ProviderLimitsEnabled  bool                     `json:"providerLimitsEnabled"`
	Precedence             []string                 `json:"precedence"`
	EligibleVariants       []RoutingEligibleVariant `json:"eligibleVariants,omitempty"`
	Warnings               []string                 `json:"warnings,omitempty"`
}

func BuildRoutingSummary(cfg *Config) RoutingSummary {
	if cfg == nil {
		return RoutingSummary{}
	}
	summary := RoutingSummary{
		ProviderPreference:     cfg.Agent.Provider,
		ABTestingEnabled:       cfg.ABTesting.EnabledValue(),
		ABTestingExplicit:      cfg.ABTesting.Enabled != nil,
		AdaptiveRoutingEnabled: cfg.Routing.Enabled,
		AutoFailoverEnabled:    cfg.Providers.AutoFailover,
		ProviderLimitsEnabled:  cfg.Providers.Limits.Enabled,
	}
	summary.Precedence = append(summary.Precedence, "agent.provider")
	if summary.ABTestingEnabled {
		summary.Precedence = append([]string{"ab_testing"}, summary.Precedence...)
	}
	if summary.AutoFailoverEnabled {
		summary.Precedence = append(summary.Precedence, "providers.auto_failover")
	}
	if summary.ProviderLimitsEnabled {
		summary.Precedence = append(summary.Precedence, "providers.limits")
	}
	if summary.AdaptiveRoutingEnabled {
		summary.Precedence = append(summary.Precedence, "routing.overlay")
	}

	hasEligible := false
	for i := range cfg.ABTesting.Experiments {
		exp := cfg.ABTesting.Experiments[i]
		if !exp.EnabledValue() {
			continue
		}
		expEligible := 0
		for j := range exp.Variants {
			v := exp.Variants[j]
			if v.Weight <= 0 {
				continue
			}
			entry := RoutingEligibleVariant{
				ExperimentID: exp.ID,
				VariantID:    v.ID,
				Provider:     v.Provider,
				Model:        v.Model,
				Eligible:     providerEnabledForRouting(cfg, v.Provider),
			}
			if !entry.Eligible {
				entry.Reason = "provider_disabled"
			} else {
				entry.Reason = "eligible"
				expEligible++
				hasEligible = true
			}
			summary.EligibleVariants = append(summary.EligibleVariants, entry)
		}
		if summary.ABTestingEnabled && expEligible == 0 {
			summary.Warnings = append(summary.Warnings, "ab_testing experiment "+exp.ID+" has zero eligible variants")
		}
	}
	if summary.ABTestingEnabled && summary.ProviderPreference != "" && len(summary.EligibleVariants) > 0 {
		summary.Warnings = append(summary.Warnings, "agent.provider is a fallback once ab_testing selects a matching variant")
	}
	if summary.ABTestingEnabled && !hasEligible {
		summary.Warnings = append(summary.Warnings, "ab_testing is enabled but no provider-enabled variants are eligible")
	}
	return summary
}

func providerEnabledForRouting(cfg *Config, provider string) bool {
	switch provider {
	case providerid.Claude:
		return cfg.Providers.Claude.Enabled
	case providerid.Codex:
		return cfg.Providers.Codex.Enabled
	case providerid.Copilot:
		return cfg.Providers.Copilot.Enabled
	case providerid.OpenCode:
		return cfg.Providers.OpenCode.Enabled
	default:
		return false
	}
}
