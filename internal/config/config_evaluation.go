package config

// EvaluationConfig controls the in-process evaluation service, which periodically
// computes a fleet scorecard (autonomy, throughput, reliability, efficiency) from
// stats + audit data. Read-only: it never dispatches agents or files issues, so
// it needs no project-type routing — each machine scores its own local data.
type EvaluationConfig struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	IntervalHours float64           `yaml:"interval_hours" json:"intervalHours"`
	WindowDays    int               `yaml:"window_days" json:"windowDays"`
	Offline       OfflineEvalConfig `yaml:"offline" json:"offline"`
}

// OfflineEvalConfig controls the offline prompt/skill eval runner
// (internal/prompteval) that screens candidate variants before they are
// eligible for online A/B enrollment.
type OfflineEvalConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Runner selects the offline runner: "auto" (default, promptfoo if on
	// PATH else native), "promptfoo", or "native".
	Runner     string `yaml:"runner" json:"runner"`
	BinaryPath string `yaml:"binary_path,omitempty" json:"binaryPath,omitempty"`
	// MinScore is the minimum Result.Score (0..1) required for a verdict to
	// be scored StatusPass.
	MinScore float64 `yaml:"min_score" json:"minScore"`
	// UnavailablePolicy is "fail" (default, fail-closed) or "pass" — what
	// AllowEnrollment returns when no verdict is recorded or the runner
	// could not measure a result.
	UnavailablePolicy string `yaml:"unavailable_policy" json:"unavailablePolicy"`
	// Mode is reserved for future dry-run/enforce distinctions; currently
	// informational only.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}
