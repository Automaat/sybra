package sybra

import (
	"cmp"
	"log/slog"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
)

// StatsService exposes statistics as Wails-bound methods.
type StatsService struct {
	stats    *stats.Store
	limits   *limits.Store
	projects *project.Store
	tasks    *task.Manager
	policy   func() limits.Policy
}

func aggregateTasksDone(resp *stats.StatsResponse, list []task.Task, now time.Time) {
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekStart := todayStart.AddDate(0, 0, -int(todayStart.Weekday()))
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	for i := range list {
		if list[i].Status != task.StatusDone {
			continue
		}

		resp.AllTime.TasksDone++

		t := taskCloseTime(list[i])

		if !t.Before(todayStart) {
			resp.Today.TasksDone++
		}
		if !t.Before(weekStart) {
			resp.ThisWeek.TasksDone++
		}
		if !t.Before(monthStart) {
			resp.ThisMonth.TasksDone++
		}
	}
}

func taskCloseTime(t task.Task) time.Time {
	if t.ClosedAt != nil {
		return *t.ClosedAt
	}
	return t.UpdatedAt
}

func closedTasksDaily(list []task.Task, now time.Time) []stats.TaskSeriesPoint {
	buckets := map[string]int{}
	for i := range list {
		if list[i].Status != task.StatusDone {
			continue
		}
		key := taskCloseTime(list[i]).In(now.Location()).Format("2006-01-02")
		buckets[key]++
	}

	days := make([]string, 0, len(buckets))
	for day := range buckets {
		days = append(days, day)
	}
	slices.Sort(days)

	out := make([]stats.TaskSeriesPoint, 0, len(days))
	for _, day := range days {
		out = append(out, stats.TaskSeriesPoint{Date: day, Count: buckets[day]})
	}
	return out
}

// GetStats returns aggregated agent run statistics.
func (s *StatsService) GetStats() stats.StatsResponse {
	if s.stats == nil {
		return stats.StatsResponse{}
	}
	now := time.Now()
	resp := s.stats.QueryAt(now)
	resp.ByProjectType = aggregateByProjectType(resp.ByProject, s.projectTypes())

	if s.tasks != nil {
		if list, err := s.tasks.List(); err == nil {
			aggregateTasksDone(&resp, list, now)
			resp.ClosedTasksDaily = closedTasksDaily(list, now)
		} else {
			slog.Warn("stats: failed to list tasks", "err", err)
		}
	}

	if s.limits != nil {
		policy := limits.DefaultPolicy()
		if s.policy != nil {
			policy = s.policy()
		}
		summary := s.limits.Summary(policy)
		resp.Limits = &summary
	}
	return resp
}

// projectTypes returns a map of project ID -> type ("pet"/"work"). An empty
// map is returned if the project store is unavailable or the listing fails;
// callers fold unknowns into the "(unknown)" bucket.
func (s *StatsService) projectTypes() map[string]string {
	if s.projects == nil {
		return map[string]string{}
	}
	list, err := s.projects.List()
	if err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(list))
	for i := range list {
		out[list[i].ID] = string(list[i].Type)
	}
	return out
}

// aggregateByProjectType folds per-project stats into per-type buckets using
// the supplied projectID -> type map. Projects absent from the map land in
// "(unknown)". Result is sorted by total cost desc to match the other groups.
func aggregateByProjectType(byProject []stats.GroupedStat, types map[string]string) []stats.GroupedStat {
	buckets := map[string]stats.Summary{}
	for _, g := range byProject {
		key, ok := types[g.Key]
		if !ok || key == "" {
			key = "(unknown)"
		}
		buckets[key] = addSummary(buckets[key], g.Stats)
	}

	out := make([]stats.GroupedStat, 0, len(buckets))
	for k, v := range buckets {
		if v.TotalRuns > 0 {
			v.AvgCostPerRun = v.TotalCostUSD / float64(v.TotalRuns)
			v.AvgDurationS = v.TotalDurationS / float64(v.TotalRuns)
		}
		out = append(out, stats.GroupedStat{Key: k, Stats: v})
	}
	slices.SortFunc(out, func(a, b stats.GroupedStat) int {
		return cmp.Compare(b.Stats.TotalCostUSD, a.Stats.TotalCostUSD)
	})
	return out
}

func addSummary(a, b stats.Summary) stats.Summary {
	return stats.Summary{
		TotalCostUSD:         a.TotalCostUSD + b.TotalCostUSD,
		TotalRuns:            a.TotalRuns + b.TotalRuns,
		TotalDurationS:       a.TotalDurationS + b.TotalDurationS,
		TotalInputTokens:     a.TotalInputTokens + b.TotalInputTokens,
		TotalOutputTokens:    a.TotalOutputTokens + b.TotalOutputTokens,
		TotalReasoningTokens: a.TotalReasoningTokens + b.TotalReasoningTokens,
		TasksDone:            a.TasksDone + b.TasksDone,
	}
}
