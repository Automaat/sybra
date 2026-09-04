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
	// MaxReportAgeHours bounds how old the persisted self-monitor report may
	// be before its findings are ignored. A "last good" report keeps driving
	// proposals via Lookback long after self-monitor stops ticking; this gate
	// treats a stale report as no evidence rather than up to LookbackHours of
	// new tasks from outdated findings.
	MaxReportAgeHours float64 `yaml:"max_report_age_hours" json:"maxReportAgeHours"`
}
