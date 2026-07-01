package config

// HarnessEvolveConfig controls the governed harness-evolution proposal loop.
// The loop proposes reviewable tasks/issues from telemetry; it never applies
// prompt, workflow, permission, retry, validator, or deployment changes itself.
type HarnessEvolveConfig struct {
	Enabled        bool    `yaml:"enabled" json:"enabled"`
	IntervalHours  float64 `yaml:"interval_hours" json:"intervalHours"`
	LookbackHours  float64 `yaml:"lookback_hours" json:"lookbackHours"`
	MinClusterSize int     `yaml:"min_cluster_size" json:"minClusterSize"`
	Sink           string  `yaml:"sink" json:"sink"`
}
