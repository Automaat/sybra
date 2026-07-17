package config

import (
	"fmt"
	"strings"
)

const (
	InstanceRoleFull      = "full"
	InstanceRoleAgentOnly = "agent-only"
)

// OrchestratorConfig gates and paces the self-starting automations. Role
// "full" runs the orchestrator brain session and the auto-dispatch scheduler,
// preserving single-node behavior. Role "agent-only" fails closed on both: it
// serves the HTTP API and runs explicitly-started agents but never orchestrates
// on its own — the posture for a secondary/test deployment (a Kubernetes
// agent-only server, a scratch instance) that must not race a full instance.
type OrchestratorConfig struct {
	// Role declares which self-starting automations this instance owns:
	// "full" (default) or "agent-only". An invalid value falls back to "full"
	// with a warning, so a typo never silently parks an instance that was meant
	// to orchestrate.
	Role string `yaml:"role,omitempty" json:"role"`
	// Enabled overrides Role for the orchestrator brain session — the
	// conversational context auto-started while tasks are active. nil (default)
	// derives from Role. Explicit true re-enables the brain on an agent-only
	// instance; explicit false parks it on a full one. Never gates an
	// operator's manual StartOrchestrator call.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled"`
	// SchedulerEnabled overrides Role for the auto-dispatch scheduler — the
	// pass that reconciles board tasks, drains the admission queue, releases
	// unblocked children, and restarts stale in-progress agents. nil (default)
	// derives from Role. Maintenance cleanup (orphan worktrees/sandboxes,
	// metrics) always runs, so a parked instance still collects its garbage.
	SchedulerEnabled *bool `yaml:"scheduler_enabled,omitempty" json:"schedulerEnabled"`
	// DispatchIntervalSeconds is the cadence of the cheap, latency-sensitive
	// dispatch pass (start the orchestrator, release unblocked children). Kept
	// short — and also fired on demand on every status change — so a
	// freshly-ready task is not left idle for a full tick. Default 10.
	DispatchIntervalSeconds int `yaml:"dispatch_interval_seconds" json:"dispatchIntervalSeconds"`
	// MaintenanceIntervalSeconds is the cadence of the expensive recovery/cleanup
	// pass (resume stalled workflows, restart stale agents, prune orphan
	// worktrees) which hits git and may spawn agents, so it must not run hot.
	// Default 60.
	MaintenanceIntervalSeconds int `yaml:"maintenance_interval_seconds" json:"maintenanceIntervalSeconds"`
	// AutoApprovePlansWithoutDecisions lets the workflow engine advance a
	// validated simple-task plan from plan-review to implementation when the
	// planner explicitly recorded that there are no open human decisions.
	// Default false.
	AutoApprovePlansWithoutDecisions bool `yaml:"auto_approve_plans_without_decisions" json:"autoApprovePlansWithoutDecisions"`
	// Pressure configures the local resource-pressure admission gate that
	// defers new agent dispatch while the host is short on disk, memory, or
	// CPU headroom. See internal/pressure.
	Pressure PressureConfig `yaml:"pressure" json:"pressure"`
}

// PressureConfig configures internal/pressure.Gate, the local
// resource-pressure admission gate consulted before dispatching new agent
// work. Thresholds are captured once at Gate construction, so a change here
// requires a restart to take effect (see diffConfig's
// "orchestrator.pressure" restart entry).
type PressureConfig struct {
	// Enabled turns the gate on. Default true. When false, New returns a nil
	// *Gate and every dispatch path admits unconditionally — the same as
	// running without this feature at all.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// MinDiskFreePercent is the minimum percentage of free disk space (on the
	// filesystem holding SYBRA_HOME) below which new dispatch is deferred.
	// <=0 disables this dimension. Default 5.
	MinDiskFreePercent float64 `yaml:"min_disk_free_percent" json:"minDiskFreePercent"`
	// MinMemAvailablePercent is the minimum percentage of available memory
	// (reclaimable caches counted as available, matching the kernel's own
	// notion of headroom) below which new dispatch is deferred. <=0 disables
	// this dimension. Default 8.
	MinMemAvailablePercent float64 `yaml:"min_mem_available_percent" json:"minMemAvailablePercent"`
	// MaxLoadPerCPU is the maximum 1-minute load average, normalized by CPU
	// count, above which new dispatch is deferred. <=0 disables this
	// dimension. Default 8.0.
	MaxLoadPerCPU float64 `yaml:"max_load_per_cpu" json:"maxLoadPerCpu"`
	// SampleIntervalSeconds is both the resource-sample cache TTL and the
	// deny-log throttle window. <=0 falls back to
	// pressure.DefaultSampleIntervalSeconds (15). Default 15.
	SampleIntervalSeconds int `yaml:"sample_interval_seconds" json:"sampleIntervalSeconds"`
}

// NormalizeInstanceRole canonicalizes an orchestrator instance role. Trim and
// lowercase first so a formatting slip ("Full", "agent-only ") never silently
// changes posture; empty maps to full; unknown values are rejected.
func NormalizeInstanceRole(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", InstanceRoleFull:
		return InstanceRoleFull, nil
	case InstanceRoleAgentOnly:
		return InstanceRoleAgentOnly, nil
	default:
		return "", fmt.Errorf("invalid orchestrator.role %q (valid: full, agent-only)", s)
	}
}

// InstanceRole returns this instance's resolved role. An invalid value is
// treated as full, so a misconfigured instance keeps the behavior it had
// before the key existed rather than silently going idle.
//
// The fallback is silent here by design: this package cannot log it usefully,
// because slog's default logger is not the server's logger — a warning emitted
// here lands at DEBUG, below the shipped level, and the operator never learns
// their role was ignored. Callers must surface it themselves by calling
// NormalizeInstanceRole (see App.applyInstanceRole). Same rationale as
// Config.ListenAddrs' envDiscarded return.
func (c OrchestratorConfig) InstanceRole() string {
	role, err := NormalizeInstanceRole(c.Role)
	if err != nil {
		return InstanceRoleFull
	}
	return role
}

// RunsOrchestrator reports whether this instance may auto-start the
// orchestrator brain session. An explicit orchestrator.enabled wins over Role.
func (c OrchestratorConfig) RunsOrchestrator() bool {
	if c.Enabled != nil {
		return *c.Enabled
	}
	return c.InstanceRole() == InstanceRoleFull
}

// RunsScheduler reports whether this instance may auto-dispatch work. An
// explicit orchestrator.scheduler_enabled wins over Role.
func (c OrchestratorConfig) RunsScheduler() bool {
	if c.SchedulerEnabled != nil {
		return *c.SchedulerEnabled
	}
	return c.InstanceRole() == InstanceRoleFull
}
