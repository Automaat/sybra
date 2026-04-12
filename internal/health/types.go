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
	CatFailureRate  Category = "failure_rate"
	CatCostOutlier  Category = "cost_outlier"
	CatStuckTask    Category = "stuck_task"
	CatWorkflowLoop Category = "workflow_loop"
	CatStatusBounce Category = "status_bounce"
	CatCostDrift    Category = "cost_drift"
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
	Findings    []Finding `json:"findings"`
	Stats       Stats     `json:"stats"`
}
