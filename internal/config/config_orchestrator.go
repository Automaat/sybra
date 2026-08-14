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
// "full" (default) runs the deterministic auto-dispatch scheduler — the
// authoritative source of core autonomy: it has sole task, workflow, queue,
// and recovery state, and advances active tasks without any LLM session in
// the loop. Role "agent-only" fails closed on scheduling: the instance serves
// the HTTP API and runs explicitly-started agents but never dispatches on its
// own — the posture for a secondary/test deployment (a Kubernetes agent-only
// server, a scratch instance) that must not race a full instance.
//
// The orchestrator brain session — a conversational LLM given a bounded
// advisory/steering role on top of the scheduler — is intentionally decoupled
// from Role and defaults off regardless of it (see Enabled). The scheduler
// already owns every state transition the brain would otherwise duplicate;
// auto-starting it is opt-in, not a Role-derived default.
type OrchestratorConfig struct {
	// Role declares which self-starting automations this instance owns:
	// "full" (default) or "agent-only". An invalid value falls back to "full"
	// with a warning, so a typo never silently parks an instance that was meant
	// to orchestrate. Only gates the scheduler (see RunsScheduler) — it has no
	// effect on the orchestrator brain (see Enabled).
	Role string `yaml:"role,omitempty" json:"role"`
	// Enabled is the sole gate for auto-starting the orchestrator brain
	// session — the conversational LLM context that would otherwise be
	// auto-started while tasks are active. nil (default) and explicit false
	// both mean disabled: an omitted key never silently restores automatic
	// startup, on any Role. Set explicit true to opt an instance into an
	// automatically-started brain. Never gates an operator's manual
	// StartOrchestrator call, which stays available regardless of this value.
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
	// WarningDiskFreePercent is the free-disk-space percentage at which
	// Sybra automatically runs a safe-cache reclaim pass (internal/cleanup's
	// RiskSafe buckets, via internal/diskreclaim), before disk free space
	// reaches MinDiskFreePercent and dispatch starts being deferred. Must be
	// set higher than MinDiskFreePercent for cleanup to get a chance to run
	// first; the gate still triggers cleanup even once MinDiskFreePercent has
	// also been crossed. <=0 disables the warning-triggered cleanup entirely.
	// Default 15.
	WarningDiskFreePercent float64 `yaml:"warning_disk_free_percent" json:"warningDiskFreePercent"`
	// RemoteMinDiskFreeBytes is the absolute emergency reserve the leader must
	// retain before it may dispatch work to a remote execution daemon. Remote
	// providers do not consume the leader's CPU or memory, so the ordinary local
	// percentage/load thresholds do not apply to them; the leader still needs
	// enough disk to persist control state and import the bounded handback. <=0
	// disables this reserve. Default 2 GiB.
	RemoteMinDiskFreeBytes int64 `yaml:"remote_min_disk_free_bytes" json:"remoteMinDiskFreeBytes"`
	// ReclaimCooldownSeconds rate-limits how often the warning-triggered safe
	// cleanup pass may run, so a host hovering right at the watermark doesn't
	// re-scan/re-delete on every dispatch tick. <=0 falls back to
	// diskreclaim.DefaultCooldown (5 minutes). Default 300.
	ReclaimCooldownSeconds int `yaml:"reclaim_cooldown_seconds" json:"reclaimCooldownSeconds"`
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
// orchestrator brain session. Decoupled from Role by design: the default is
// always false, on every role, so a legacy config, a v2 config that only sets
// Role, or any config that simply omits the key never silently resumes
// automatic brain startup. Only an explicit orchestrator.enabled: true opts
// in.
func (c OrchestratorConfig) RunsOrchestrator() bool {
	if c.Enabled != nil {
		return *c.Enabled
	}
	return false
}

// RunsScheduler reports whether this instance may auto-dispatch work. An
// explicit orchestrator.scheduler_enabled wins over Role.
func (c OrchestratorConfig) RunsScheduler() bool {
	if c.SchedulerEnabled != nil {
		return *c.SchedulerEnabled
	}
	return c.InstanceRole() == InstanceRoleFull
}
