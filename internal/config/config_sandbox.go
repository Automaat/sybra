package config

// SandboxConfig controls retention of per-task app sandbox dirs under
// ~/.sybra/sandboxes (see internal/sandbox.Manager.CleanupOrphaned).
type SandboxConfig struct {
	// RetentionHours bounds how long a done/cancelled/blocked task's sandbox
	// dir survives before the periodic sweep removes it. 0 falls back to
	// DefaultSandboxRetention (24h); a negative value disables age-based
	// pruning (eligible dirs are never removed by age, only on task delete).
	RetentionHours int `yaml:"retention_hours" json:"retentionHours"`
	// BuildCacheIdleHours is how long a per-task Go build cache may sit
	// untouched before cleanup may reclaim it regardless of its owning
	// task's status. 0 falls back to DefaultBuildCacheIdleHours; negative
	// disables idle reclaim and leaves these caches on task status alone.
	//
	// Separate from RetentionHours because it answers a different question.
	// Retention asks how long after a task finishes its resources are kept;
	// this asks how long an unused cache is worth its disk. A task parked in
	// human-required never finishes, so retention never starts running, and
	// its cache is pinned indefinitely — which is what filled the disk.
	BuildCacheIdleHours int `yaml:"build_cache_idle_hours" json:"buildCacheIdleHours"`
}
