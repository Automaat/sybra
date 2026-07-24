package config

// MonitorConfig controls the in-process monitor service that replaces the
// /loop 5m /sybra-monitor skill. Each tick snapshots the board + audit
// window, detects anomalies (lost agents, PR gaps, dwell, failure spikes,
// bottlenecks), runs idempotent remediations directly, and dispatches a
// focused headless agent for anomalies that need LLM judgment.
type MonitorConfig struct {
	Enabled              bool               `yaml:"enabled" json:"enabled"`
	IntervalSeconds      int                `yaml:"interval_seconds" json:"intervalSeconds"`
	Model                string             `yaml:"model" json:"model"`
	IssueCooldownMinutes int                `yaml:"issue_cooldown_minutes" json:"issueCooldownMinutes"`
	DispatchLimit        int                `yaml:"dispatch_limit" json:"dispatchLimit"`
	StuckHumanHours      float64            `yaml:"stuck_human_hours" json:"stuckHumanHours"`
	LostAgentMinutes     int                `yaml:"lost_agent_minutes" json:"lostAgentMinutes"`
	PRGapGraceMinutes    int                `yaml:"pr_gap_grace_minutes" json:"prGapGraceMinutes"`
	FailureRateThreshold float64            `yaml:"failure_rate_threshold" json:"failureRateThreshold"`
	BottleneckHours      map[string]float64 `yaml:"bottleneck_hours" json:"bottleneckHours"`
	IssueLabel           string             `yaml:"issue_label" json:"issueLabel"`
	IssueRepo            string             `yaml:"issue_repo" json:"issueRepo"`
	// LostAgentIssueAfterOccurrences is how many consecutive ticks a
	// lost_agent anomaly must be detected for the same task before an issue
	// is filed. The deterministic remediation (resetLostAgent) runs every
	// tick regardless; a single recurrence just means recovery hasn't taken
	// effect yet, not that it failed.
	LostAgentIssueAfterOccurrences int `yaml:"lost_agent_issue_after_occurrences" json:"lostAgentIssueAfterOccurrences"`
	// LostAgentAutoCloseAfterClears is how many consecutive ticks a
	// previously-filed lost_agent issue's task must stay clear (no longer
	// detected as lost) before the issue is auto-closed.
	LostAgentAutoCloseAfterClears int `yaml:"lost_agent_auto_close_after_clears" json:"lostAgentAutoCloseAfterClears"`
}
