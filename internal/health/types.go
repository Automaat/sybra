package health

import "time"

// Severity indicates how urgent a finding is.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Category classifies what a finding is about.
type Category string

const (
	CatFailureRate      Category = "failure_rate"
	CatCostOutlier      Category = "cost_outlier"
	CatStuckTask        Category = "stuck_task"
	CatWorkflowLoop     Category = "workflow_loop"
	CatStatusBounce     Category = "status_bounce"
	CatCostDrift        Category = "cost_drift"
	CatAgentRetryLoop   Category = "agent_retry_loop"
	CatTriageMismatch   Category = "triage_mismatch"
	CatStatusBottleneck Category = "status_bottleneck"
)

// Score is the rollup verdict across all findings in a report.
type Score string

const (
	ScoreGood     Score = "good"
	ScoreWarning  Score = "warning"
	ScoreCritical Score = "critical"
)

// Finding is a single health issue detected by the checker.
type Finding struct {
	Category    Category       `json:"category"`
	Severity    Severity       `json:"severity"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	TaskID      string         `json:"taskId,omitempty"`
	AgentID     string         `json:"agentId,omitempty"`
	Role        string         `json:"role,omitempty"`
	LogFile     string         `json:"logFile,omitempty"`
	Evidence    map[string]any `json:"evidence"`
	DetectedAt  time.Time      `json:"detectedAt"`
	// Fingerprint is a stable dedup key derived from (Category, TaskID,
	// Evidence). Populated by the Checker after each tick; downstream
	// consumers (selfmonitor, issue sinks) use it as a cross-run identity.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Stats aggregates basic metrics for the check window.
type Stats struct {
	TotalAgentRuns  int                `json:"totalAgentRuns"`
	FailedAgentRuns int                `json:"failedAgentRuns"`
	FailureRate     float64            `json:"failureRate"`
	CostByRole      map[string]float64 `json:"costByRole"`
	TotalCostUSD    float64            `json:"totalCostUsd"`
}

// Report is the output of a single health check run.
type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`
	PeriodStart time.Time `json:"periodStart"`
	PeriodEnd   time.Time `json:"periodEnd"`
	Score       Score     `json:"score"`
	Findings    []Finding `json:"findings"`
	Stats       Stats     `json:"stats"`
}

// RollupScore returns good if there are no findings, critical if any finding
// is critical, otherwise warning.
func RollupScore(findings []Finding) Score {
	if len(findings) == 0 {
		return ScoreGood
	}
	for i := range findings {
		if findings[i].Severity == SeverityCritical {
			return ScoreCritical
		}
	}
	return ScoreWarning
}
