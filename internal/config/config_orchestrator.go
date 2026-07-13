package config

type OrchestratorConfig struct {
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
