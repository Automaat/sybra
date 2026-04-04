package main

import "github.com/Automaat/synapse/internal/stats"

func (a *App) GetStats() stats.StatsResponse {
	if a.stats == nil {
		return stats.StatsResponse{}
	}
	return a.stats.Query()
}
