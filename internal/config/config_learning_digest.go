package config

// LearningDigestConfig controls the periodic Learning Digest service
// (internal/learning), which distills the evaluation scorecard, stats, audit
// signals, and active A/B experiments into a retrospective of what worked,
// what didn't, and what to try next. Default-disabled: operators opt in
// explicitly, mirroring EvaluationConfig.
type LearningDigestConfig struct {
	Enabled       bool    `yaml:"enabled" json:"enabled"`
	IntervalHours float64 `yaml:"interval_hours" json:"intervalHours"`
	WindowDays    int     `yaml:"window_days" json:"windowDays"`
	// MaxWindowDays caps how far back a cold-start or long-idle window can
	// reach, so a fresh install or resumed-after-months instance never feeds
	// the summarizer an unbounded prompt.
	MaxWindowDays int `yaml:"max_window_days" json:"maxWindowDays"`
	// Model is passed to the claude CLI (the digest summarizer runs
	// claude-only — see internal/learning/digest.go).
	Model string `yaml:"model" json:"model"`
	// MinRuns and MinLandings gate generation: a digest is only produced when
	// the window has at least this much fresh signal, so an idle fleet does
	// not produce an empty/noisy retrospective.
	MinRuns     int `yaml:"min_runs" json:"minRuns"`
	MinLandings int `yaml:"min_landings" json:"minLandings"`
}
