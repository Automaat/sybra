// Package monitor runs the in-process anomaly detector + remediator that
// replaces the legacy /loop 5m /synapse-monitor skill. The Service ticks every
// MonitorConfig.IntervalSeconds, snapshots the board + audit window, runs a
// pure Detect pass, applies idempotent remediations directly, and dispatches
// a focused headless agent for the anomalies that need LLM judgment.
package monitor

import "time"

type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// AnomalyKind is the canonical name of a detection rule. Every Anomaly carries
// exactly one Kind; the dispatcher and issue sink use it to choose templates.
type AnomalyKind string

const (
	KindOverDispatchLimit AnomalyKind = "over_dispatch_limit"
	KindStuckHumanBlocked AnomalyKind = "stuck_human_blocked"
	KindUntriaged         AnomalyKind = "untriaged"
	KindPRGap             AnomalyKind = "pr_gap"
	KindLostAgent         AnomalyKind = "lost_agent"
	KindFailureSpike      AnomalyKind = "failure_spike"
	KindBottleneck        AnomalyKind = "bottleneck"
)

// AllAnomalyKinds returns every kind in declaration order. Useful for tests
// and for building exhaustive switches that the compiler cannot enforce.
func AllAnomalyKinds() []AnomalyKind {
	return []AnomalyKind{
		KindOverDispatchLimit,
		KindStuckHumanBlocked,
		KindUntriaged,
		KindPRGap,
		KindLostAgent,
		KindFailureSpike,
		KindBottleneck,
	}
}

// Anomaly is a single detected drift from workflow rules. It is data — no
// behaviour — and is safe to serialize over Wails events or print as JSON.
type Anomaly struct {
	Kind        AnomalyKind    `json:"kind"`
	TaskID      string         `json:"taskId,omitempty"`
	Severity    Severity       `json:"severity"`
	RequiresLLM bool           `json:"requiresLlm"`
	Fingerprint string         `json:"fingerprint"`
	Evidence    map[string]any `json:"evidence,omitempty"`
	DetectedAt  time.Time      `json:"detectedAt"`
}

// Counts is a snapshot of board status counts at the moment of detection.
// Keys are the task.Status string values; the explicit fields exist for the
// common ones the summary line prints.
type Counts struct {
	New           int            `json:"new"`
	Todo          int            `json:"todo"`
	InProgress    int            `json:"inProgress"`
	InReview      int            `json:"inReview"`
	PlanReview    int            `json:"planReview"`
	HumanRequired int            `json:"humanRequired"`
	Done          int            `json:"done"`
	ByStatus      map[string]int `json:"byStatus"`
}

// Report is the result of one tick. Anomalies is the raw detector output;
// Remediated/Dispatched/Issues are filled in by the Service after side effects.
type Report struct {
	GeneratedAt   time.Time `json:"generatedAt"`
	Counts        Counts    `json:"counts"`
	Anomalies     []Anomaly `json:"anomalies"`
	Remediated    []string  `json:"remediated"`
	Dispatched    []string  `json:"dispatched"`
	IssuesOpened  int       `json:"issuesOpened"`
	IssuesUpdated int       `json:"issuesUpdated"`
}
