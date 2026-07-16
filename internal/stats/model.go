package stats

import (
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

// Outcome values recorded on RunRecord.Outcome.
//
// OutcomeStalled marks a run that never produced a definitive result — a signal
// kill, a stop before the terminal event, a provider rate limit, or a malformed
// tool call (see completion.classifyStall). Sybra retries those runs, so a stall
// is neither a success nor a failure: it must stay out of both the numerator and
// the denominator of any failure rate, or a provider that stalls often reads as
// more reliable than one that stalls rarely. Use IsTerminalOutcome to gate a
// record into an outcome-derived rate.
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
	OutcomeStalled   = "stalled"
)

// IsTerminalOutcome reports whether an outcome represents a definitive result
// and therefore belongs in a failure-rate denominator.
//
// Deliberately an allowlist. A denylist ("anything but stalled") would quietly
// count an empty, corrupt, or newly-added outcome as a resolved non-failure —
// the same false dichotomy that caused this bug, where runOutcome assumed any
// non-nil exit error meant failure. Anything not known to be definitive is not
// definitive.
func IsTerminalOutcome(outcome string) bool {
	return outcome == OutcomeCompleted || outcome == OutcomeFailed
}

// RunRecord captures a single agent execution for analytics.
//
// Cache token fields are recorded so the stats UI can show actual API
// consumption — for long Claude runs cache reads dominate by 100–10000×
// (e.g. 1.07M cache reads vs 24 uncached input tokens in a single result
// event), so plain InputTokens alone looks misleadingly small.
type RunRecord struct {
	ID                       string  `json:"id"`
	TaskID                   string  `json:"taskId"`
	ProjectID                string  `json:"projectId,omitempty"`
	Mode                     string  `json:"mode"`
	Role                     string  `json:"role"`
	Model                    string  `json:"model,omitempty"`
	Provider                 string  `json:"provider,omitempty"`
	ReasoningEffort          string  `json:"reasoningEffort,omitempty"`
	ExperimentID             string  `json:"experimentId,omitempty"`
	VariantID                string  `json:"variantId,omitempty"`
	AssignmentUnit           string  `json:"assignmentUnit,omitempty"`
	AssignmentKey            string  `json:"assignmentKey,omitempty"`
	CostUSD                  float64 `json:"costUsd"`
	DurationS                float64 `json:"durationS"`
	InputTokens              int     `json:"inputTokens,omitempty"`
	OutputTokens             int     `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	ReasoningTokens          int     `json:"reasoningTokens,omitempty"`
	PremiumRequests          float64 `json:"premiumRequests,omitempty"`
	// TurnCount and ToolCalls capture per-run effort so the evaluation
	// scorecard can measure convergence (turns per landed PR) and tool
	// efficiency. Zero for runs recorded before these were tracked.
	TurnCount int       `json:"turnCount,omitempty"`
	ToolCalls int       `json:"toolCalls,omitempty"`
	Outcome   string    `json:"outcome"`
	Timestamp time.Time `json:"timestamp"`
}

// Summary holds aggregate metrics over a set of runs.
type Summary struct {
	TotalCostUSD                  float64 `json:"totalCostUsd"`
	TotalRuns                     int     `json:"totalRuns"`
	AvgCostPerRun                 float64 `json:"avgCostPerRun"`
	AvgDurationS                  float64 `json:"avgDurationS"`
	TotalDurationS                float64 `json:"totalDurationS"`
	TotalInputTokens              int     `json:"totalInputTokens"`
	TotalOutputTokens             int     `json:"totalOutputTokens"`
	TotalCacheCreationInputTokens int     `json:"totalCacheCreationInputTokens,omitempty"`
	TotalCacheReadInputTokens     int     `json:"totalCacheReadInputTokens,omitempty"`
	TotalReasoningTokens          int     `json:"totalReasoningTokens,omitempty"`
	TotalPremiumRequests          float64 `json:"totalPremiumRequests,omitempty"`
	TasksDone                     int     `json:"tasksDone"`
}

// GroupedStat is an aggregate keyed by a dimension (project, mode, etc).
type GroupedStat struct {
	Key   string  `json:"key"`
	Stats Summary `json:"stats"`
}

// TaskSeriesPoint is a daily task-count bucket for time-series charts.
type TaskSeriesPoint struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// StatsResponse is the full analytics payload returned to the frontend.
type StatsResponse struct {
	Today            Summary           `json:"today"`
	ThisWeek         Summary           `json:"thisWeek"`
	ThisMonth        Summary           `json:"thisMonth"`
	AllTime          Summary           `json:"allTime"`
	ByProject        []GroupedStat     `json:"byProject"`
	ByProjectType    []GroupedStat     `json:"byProjectType"`
	ByMode           []GroupedStat     `json:"byMode"`
	ByRole           []GroupedStat     `json:"byRole"`
	ByModel          []GroupedStat     `json:"byModel"`
	ByProvider       []GroupedStat     `json:"byProvider"`
	RecentRuns       []RunRecord       `json:"recentRuns"`
	ClosedTasksDaily []TaskSeriesPoint `json:"closedTasksDaily"`
	Limits           *limits.Summary   `json:"limits,omitempty"`
}
