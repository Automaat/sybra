package sybra

import "github.com/Automaat/sybra/internal/stats"

// StatsService exposes statistics as Wails-bound methods.
type StatsService struct {
	stats *stats.Store
}

// GetStats returns aggregated agent run statistics.
func (s *StatsService) GetStats() stats.StatsResponse {
	if s.stats == nil {
		return stats.StatsResponse{}
	}
	return s.stats.Query()
}
