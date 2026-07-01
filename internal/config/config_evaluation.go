package config

// EvaluationConfig controls the in-process evaluation service, which periodically
// computes a fleet scorecard (autonomy, throughput, reliability, efficiency) from
// stats + audit data. Read-only: it never dispatches agents or files issues, so
// it needs no project-type routing — each machine scores its own local data.
type EvaluationConfig struct {
	Enabled       bool    `yaml:"enabled" json:"enabled"`
	IntervalHours float64 `yaml:"interval_hours" json:"intervalHours"`
	WindowDays    int     `yaml:"window_days" json:"windowDays"`
}
