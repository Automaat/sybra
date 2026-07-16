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
	// HTTPErrorRateThreshold is the sybra-server 5xx-response-rate SLO
	// target: a rolling window with a higher fraction of 5xx responses than
	// this trips a KindHTTPErrorSpike anomaly. Evaluated only once the
	// window has at least HTTPErrorRateMinRequests samples, so low-traffic
	// periods can't trigger on a single failed request.
	HTTPErrorRateThreshold float64 `yaml:"http_error_rate_threshold" json:"httpErrorRateThreshold"`
	// HTTPErrorRateWindowMinutes is the rolling window width the threshold
	// above is evaluated over.
	HTTPErrorRateWindowMinutes int `yaml:"http_error_rate_window_minutes" json:"httpErrorRateWindowMinutes"`
	// HTTPErrorRateMinRequests is the minimum request volume the window
	// must have before the error-rate threshold is evaluated.
	HTTPErrorRateMinRequests int `yaml:"http_error_rate_min_requests" json:"httpErrorRateMinRequests"`
}
