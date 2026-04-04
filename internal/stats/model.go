package stats

import "time"

// RunRecord captures a single agent execution for analytics.
type RunRecord struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"taskId"`
	ProjectID    string    `json:"projectId,omitempty"`
	Mode         string    `json:"mode"`
	Role         string    `json:"role"`
	Model        string    `json:"model,omitempty"`
	CostUSD      float64   `json:"costUsd"`
	DurationS    float64   `json:"durationS"`
	InputTokens  int       `json:"inputTokens,omitempty"`
	OutputTokens int       `json:"outputTokens,omitempty"`
	Outcome      string    `json:"outcome"`
	Timestamp    time.Time `json:"timestamp"`
}

// Summary holds aggregate metrics over a set of runs.
type Summary struct {
	TotalCostUSD      float64 `json:"totalCostUsd"`
	TotalRuns         int     `json:"totalRuns"`
	AvgCostPerRun     float64 `json:"avgCostPerRun"`
	AvgDurationS      float64 `json:"avgDurationS"`
	TotalDurationS    float64 `json:"totalDurationS"`
	TotalInputTokens  int     `json:"totalInputTokens"`
	TotalOutputTokens int     `json:"totalOutputTokens"`
}

// GroupedStat is an aggregate keyed by a dimension (project, mode, etc).
type GroupedStat struct {
	Key   string  `json:"key"`
	Stats Summary `json:"stats"`
}

// StatsResponse is the full analytics payload returned to the frontend.
type StatsResponse struct {
	Today      Summary       `json:"today"`
	ThisWeek   Summary       `json:"thisWeek"`
	ThisMonth  Summary       `json:"thisMonth"`
	AllTime    Summary       `json:"allTime"`
	ByProject  []GroupedStat `json:"byProject"`
	ByMode     []GroupedStat `json:"byMode"`
	ByRole     []GroupedStat `json:"byRole"`
	ByModel    []GroupedStat `json:"byModel"`
	RecentRuns []RunRecord   `json:"recentRuns"`
}
