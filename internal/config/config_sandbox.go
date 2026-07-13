package config

// SandboxConfig controls retention of per-task app sandbox dirs under
// ~/.sybra/sandboxes (see internal/sandbox.Manager.CleanupOrphaned).
type SandboxConfig struct {
	// RetentionHours bounds how long a done/cancelled/blocked task's sandbox
	// dir survives before the periodic sweep removes it. 0 falls back to
	// DefaultSandboxRetention (24h); a negative value disables age-based
	// pruning (eligible dirs are never removed by age, only on task delete).
	RetentionHours int `yaml:"retention_hours" json:"retentionHours"`
}
