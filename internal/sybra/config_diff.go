package sybra

import (
	"reflect"

	"github.com/Automaat/sybra/internal/config"
)

func appendDeepChanged(keys []string, name string, old, next any) []string {
	if !reflect.DeepEqual(old, next) {
		return append(keys, name)
	}
	return keys
}

// diffConfig compares old and new configs and returns two slices of dot-separated
// key names: hot (live-reloadable without restart) and restart (require restart).
func diffConfig(old, next config.Config) (hot, restart []string) {
	// Hot-reloadable individual fields
	if old.Notification.Desktop != next.Notification.Desktop {
		hot = append(hot, "notification.desktop")
	}
	if old.Agent.MaxConcurrent != next.Agent.MaxConcurrent {
		hot = append(hot, "agent.max_concurrent")
	}
	if old.Agent.Provider != next.Agent.Provider {
		hot = append(hot, "agent.provider")
	}
	if old.Agent.MaxCostUSD != next.Agent.MaxCostUSD {
		hot = append(hot, "agent.max_cost_usd")
	}
	if old.Agent.MaxTurns != next.Agent.MaxTurns {
		hot = append(hot, "agent.max_turns")
	}
	if old.Agent.TurnCostFraction != next.Agent.TurnCostFraction {
		hot = append(hot, "agent.turn_cost_fraction")
	}
	if old.Agent.TurnMultiplier != next.Agent.TurnMultiplier {
		hot = append(hot, "agent.turn_multiplier")
	}
	if old.Agent.BashTimeoutSeconds != next.Agent.BashTimeoutSeconds {
		hot = append(hot, "agent.bash_timeout_seconds")
	}
	if old.Agent.RetryWatchdog != next.Agent.RetryWatchdog {
		hot = append(hot, "agent.retry_watchdog")
	}
	if old.Agent.FallbackModel != next.Agent.FallbackModel {
		hot = append(hot, "agent.fallback_model")
	}
	if old.Logging.Level != next.Logging.Level {
		hot = append(hot, "logging.level")
	}
	if !reflect.DeepEqual(old.Todoist, next.Todoist) {
		hot = append(hot, "todoist")
	}
	if old.Renovate.Author != next.Renovate.Author || old.Renovate.Enabled != next.Renovate.Enabled {
		hot = append(hot, "renovate")
	}

	// Other agent fields that don't require restart but have no live setter
	if old.Agent.Model != next.Agent.Model ||
		old.Agent.Mode != next.Agent.Mode ||
		old.Agent.ResearchMachineDir != next.Agent.ResearchMachineDir ||
		old.Agent.MaxLogEvents != next.Agent.MaxLogEvents ||
		old.Agent.LogRetentionDays != next.Agent.LogRetentionDays ||
		old.Agent.RequirePermissions != next.Agent.RequirePermissions {
		hot = append(hot, "agent.other")
	}

	// Orchestrator AutoTriage/AutoPlan are live-effective via pointer read; the
	// dispatch/maintenance intervals are sampled once at loop start (NewTicker),
	// so changing them needs a restart to take effect.
	if old.Orchestrator.AutoTriage != next.Orchestrator.AutoTriage ||
		old.Orchestrator.AutoPlan != next.Orchestrator.AutoPlan {
		hot = append(hot, "orchestrator")
	}
	if old.Orchestrator.DispatchIntervalSeconds != next.Orchestrator.DispatchIntervalSeconds ||
		old.Orchestrator.MaintenanceIntervalSeconds != next.Orchestrator.MaintenanceIntervalSeconds {
		restart = append(restart, "orchestrator.intervals")
	}
	if !reflect.DeepEqual(old.Audit, next.Audit) {
		hot = append(hot, "audit")
	}
	if old.Logging.MaxSizeMB != next.Logging.MaxSizeMB || old.Logging.MaxFiles != next.Logging.MaxFiles {
		hot = append(hot, "logging.other")
	}

	// Restart-required blocks
	restart = appendDeepChanged(restart, "providers", old.Providers, next.Providers)
	restart = appendDeepChanged(restart, "metrics", old.Metrics, next.Metrics)
	restart = appendDeepChanged(restart, "monitor", old.Monitor, next.Monitor)
	restart = appendDeepChanged(restart, "self_monitor", old.SelfMonitor, next.SelfMonitor)
	restart = appendDeepChanged(restart, "harness_evolution", old.HarnessEvolve, next.HarnessEvolve)
	restart = appendDeepChanged(restart, "prompt_lab", old.PromptLab, next.PromptLab)
	restart = appendDeepChanged(restart, "ab_testing", old.ABTesting, next.ABTesting)
	restart = appendDeepChanged(restart, "triage", old.Triage, next.Triage)
	restart = appendDeepChanged(restart, "github", old.GitHub, next.GitHub)
	restart = appendDeepChanged(restart, "project_types", old.ProjectTypes, next.ProjectTypes)
	restart = appendDeepChanged(restart, "auto_update", old.AutoUpdate, next.AutoUpdate)
	restart = appendDeepChanged(restart, "browser", old.Browser, next.Browser)

	return hot, restart
}
