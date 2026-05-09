package sybra

import (
	"cmp"
	"slices"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/stats"
)

// StatsService exposes statistics as Wails-bound methods.
type StatsService struct {
	stats    *stats.Store
	projects *project.Store
}

// GetStats returns aggregated agent run statistics.
func (s *StatsService) GetStats() stats.StatsResponse {
	if s.stats == nil {
		return stats.StatsResponse{}
	}
	resp := s.stats.Query()
	resp.ByProjectType = aggregateByProjectType(resp.ByProject, s.projectTypes())
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
	}
}
