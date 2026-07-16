package health

import (
	"time"

	"github.com/Automaat/sybra/internal/procstat"
)

// Severity indicates how urgent a finding is.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Category classifies what a finding is about.
type Category string

const (
	CatFailureRate       Category = "failure_rate"
	CatCostOutlier       Category = "cost_outlier"
	CatDockerReclaimable Category = "docker_reclaimable"
	CatStuckTask         Category = "stuck_task"
	CatWorkflowLoop      Category = "workflow_loop"
	CatStatusBounce      Category = "status_bounce"
	CatCostDrift         Category = "cost_drift"
	CatAgentRetryLoop    Category = "agent_retry_loop"
	CatTriageMismatch    Category = "triage_mismatch"
	CatStatusBottleneck  Category = "status_bottleneck"
	CatGHAuthFailure     Category = "gh_auth_failure"
	CatSandboxCleanup    Category = "sandbox_cleanup_failure"
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

// DockerDiskUsage is the operator-facing Docker reclaimable-disk sample.
type DockerDiskUsage struct {
	Available        bool      `json:"available"`
	ReclaimableBytes int64     `json:"reclaimableBytes"`
	TotalBytes       int64     `json:"totalBytes,omitempty"`
	ManualCommand    string    `json:"manualCommand,omitempty"`
	SampledAt        time.Time `json:"sampledAt"`
}

// ReclaimStatus summarizes the last automatic safe-cache reclaim pass.
type ReclaimStatus struct {
	RanAt              time.Time `json:"ranAt"`
	ReclaimedBytes     int64     `json:"reclaimedBytes"`
	UnreclaimableBytes int64     `json:"unreclaimableBytes"`
	Errors             int       `json:"errors"`
}

// PressureStatus captures the current pressure sample plus the most recent
// automatic safe-cache reclaim pass, if any.
type PressureStatus struct {
	// DiskFreePct, MemAvailablePct, and LoadPerCPU are -1 when the
	// underlying signal could not be sampled (see
	// internal/pressure.Sample). The source reading is NaN in that case,
	// but encoding/json cannot marshal NaN — the Checker sanitizes it to
	// -1 (never a legitimate value for these fields) before persisting so
	// one unreadable signal can't blackhole the whole report.
	DiskFreePct         float64        `json:"diskFreePct"`
	MemAvailablePct     float64        `json:"memAvailablePct"`
	LoadPerCPU          float64        `json:"loadPerCpu"`
	WarningDiskFreePct  float64        `json:"warningDiskFreePct"`
	CriticalDiskFreePct float64        `json:"criticalDiskFreePct"`
	LastReclaim         *ReclaimStatus `json:"lastReclaim,omitempty"`
}

// Report is the output of a single health check run.
type Report struct {
	GeneratedAt time.Time         `json:"generatedAt"`
	PeriodStart time.Time         `json:"periodStart"`
	PeriodEnd   time.Time         `json:"periodEnd"`
	Score       Score             `json:"score"`
	Findings    []Finding         `json:"findings"`
	Stats       Stats             `json:"stats"`
	Docker      *DockerDiskUsage  `json:"docker,omitempty"`
	Pressure    *PressureStatus   `json:"pressure,omitempty"`
	Processes   *procstat.Summary `json:"processes,omitempty"`
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
