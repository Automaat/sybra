package sybra

import (
	"reflect"

	"github.com/Automaat/sybra/internal/config"
)

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

	// Orchestrator and audit are live-effective via pointer read; no setter needed
	if !reflect.DeepEqual(old.Orchestrator, next.Orchestrator) {
		hot = append(hot, "orchestrator")
	}
	if !reflect.DeepEqual(old.Audit, next.Audit) {
		hot = append(hot, "audit")
	}
	if old.Logging.MaxSizeMB != next.Logging.MaxSizeMB || old.Logging.MaxFiles != next.Logging.MaxFiles {
		hot = append(hot, "logging.other")
	}

	// Restart-required blocks
	if !reflect.DeepEqual(old.Providers, next.Providers) {
		restart = append(restart, "providers")
	}
	if !reflect.DeepEqual(old.Metrics, next.Metrics) {
		restart = append(restart, "metrics")
	}
	if !reflect.DeepEqual(old.Monitor, next.Monitor) {
		restart = append(restart, "monitor")
	}
	if !reflect.DeepEqual(old.SelfMonitor, next.SelfMonitor) {
		restart = append(restart, "self_monitor")
	}
	if !reflect.DeepEqual(old.Triage, next.Triage) {
		restart = append(restart, "triage")
	}
	if !reflect.DeepEqual(old.GitHub, next.GitHub) {
		restart = append(restart, "github")
	}
	if !reflect.DeepEqual(old.ProjectTypes, next.ProjectTypes) {
		restart = append(restart, "project_types")
	}

	return hot, restart
}
