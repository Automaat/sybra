package config

// SelfMonitorConfig controls the in-process selfmonitor service that
// replaces the /loop 6h /sybra-self-monitor skill. Each tick snapshots
// the latest health report, distills per-finding agent logs into a
// LogSummary, runs a two-stage LLM judge + synthesizer (Phase C), files
// deduped issues via the shared monitor.IssueSink, and autonomously
// remediates a whitelisted set of categories (Phase D). Enabled stays
// false until users opt in.
type SelfMonitorConfig struct {
	Enabled              bool     `yaml:"enabled" json:"enabled"`
	IntervalHours        float64  `yaml:"interval_hours" json:"intervalHours"`
	JudgeModel           string   `yaml:"judge_model" json:"judgeModel"`
	SynthesizerModel     string   `yaml:"synthesizer_model" json:"synthesizerModel"`
	MaxIssuesPerRun      int      `yaml:"max_issues_per_run" json:"maxIssuesPerRun"`
	MaxAutoActionsPerDay int      `yaml:"max_auto_actions_per_day" json:"maxAutoActionsPerDay"`
	AutoActCategories    []string `yaml:"auto_act_categories" json:"autoActCategories"`
	DryRun               bool     `yaml:"dry_run" json:"dryRun"`
	IssueCooldownHours   float64  `yaml:"issue_cooldown_hours" json:"issueCooldownHours"`
	IssueLabel           string   `yaml:"issue_label" json:"issueLabel"`
	MaxCostPerTickUSD    float64  `yaml:"max_cost_per_tick_usd" json:"maxCostPerTickUsd"`
	JudgeParallelism     int      `yaml:"judge_parallelism" json:"judgeParallelism"`
	SuppressionDays      int      `yaml:"suppression_days" json:"suppressionDays"`
	SuppressionThreshold int      `yaml:"suppression_threshold" json:"suppressionThreshold"`
}
