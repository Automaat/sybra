package stats

import (
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

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
	CostUSD                  float64 `json:"costUsd"`
	DurationS                float64 `json:"durationS"`
	InputTokens              int     `json:"inputTokens,omitempty"`
	OutputTokens             int     `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	ReasoningTokens          int     `json:"reasoningTokens,omitempty"`
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
}

// GroupedStat is an aggregate keyed by a dimension (project, mode, etc).
type GroupedStat struct {
	Key   string  `json:"key"`
	Stats Summary `json:"stats"`
}

// StatsResponse is the full analytics payload returned to the frontend.
type StatsResponse struct {
	Today         Summary        `json:"today"`
	ThisWeek      Summary        `json:"thisWeek"`
	ThisMonth     Summary        `json:"thisMonth"`
	AllTime       Summary        `json:"allTime"`
	ByProject     []GroupedStat  `json:"byProject"`
	ByProjectType []GroupedStat  `json:"byProjectType"`
	ByMode        []GroupedStat  `json:"byMode"`
	ByRole        []GroupedStat  `json:"byRole"`
	ByModel       []GroupedStat  `json:"byModel"`
	ByProvider    []GroupedStat  `json:"byProvider"`
	RecentRuns    []RunRecord    `json:"recentRuns"`
	Limits        limits.Summary `json:"limits"`
}
