package config

// TrashConfig controls the retention of soft-deleted tasks under
// ~/.sybra/trash (see internal/task.Store.Delete).
type TrashConfig struct {
	// RetentionDays bounds how long a trashed task generation survives
	// before the startup sweep permanently removes it. 0 falls back to
	// DefaultTrashRetentionDays (14); a negative value disables pruning.
	RetentionDays int `yaml:"retention_days" json:"retentionDays"`
}
