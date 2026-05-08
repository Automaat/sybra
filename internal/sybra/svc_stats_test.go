package sybra

import (
	"math"
	"testing"

	"github.com/Automaat/sybra/internal/stats"
)

func TestAggregateByProjectType(t *testing.T) {
	byProject := []stats.GroupedStat{
		{Key: "Automaat/sybra", Stats: stats.Summary{
			TotalCostUSD: 1.0, TotalRuns: 4, TotalDurationS: 200,
			TotalInputTokens: 1000, TotalOutputTokens: 500,
			AvgCostPerRun: 0.25, AvgDurationS: 50,
		}},
		{Key: "Automaat/zsh-clean-history", Stats: stats.Summary{
			TotalCostUSD: 0.5, TotalRuns: 2, TotalDurationS: 100,
			AvgCostPerRun: 0.25, AvgDurationS: 50,
		}},
		{Key: "kumahq/kuma", Stats: stats.Summary{
			TotalCostUSD: 3.0, TotalRuns: 6, TotalDurationS: 600,
			AvgCostPerRun: 0.5, AvgDurationS: 100,
		}},
		{Key: "no/such-project", Stats: stats.Summary{
			TotalCostUSD: 0.1, TotalRuns: 1, TotalDurationS: 10,
			AvgCostPerRun: 0.1, AvgDurationS: 10,
		}},
	}
	types := map[string]string{
		"Automaat/sybra":             "pet",
		"Automaat/zsh-clean-history": "pet",
		"kumahq/kuma":                "work",
	}

	out := aggregateByProjectType(byProject, types)

	if len(out) != 3 {
		t.Fatalf("want 3 buckets (pet, work, (unknown)); got %d: %+v", len(out), out)
	}

	// Sorted by cost desc: work (3.0) > pet (1.5) > (unknown) (0.1)
	if out[0].Key != "work" || out[1].Key != "pet" || out[2].Key != "(unknown)" {
		t.Errorf("ordering: got %s,%s,%s; want work,pet,(unknown)", out[0].Key, out[1].Key, out[2].Key)
	}

	pet := find(t, out, "pet")
	if pet.TotalRuns != 6 {
		t.Errorf("pet.totalRuns: got %d, want 6", pet.TotalRuns)
	}
	if !nearly(pet.TotalCostUSD, 1.5) {
		t.Errorf("pet.totalCost: got %f, want 1.5", pet.TotalCostUSD)
	}
	if !nearly(pet.AvgCostPerRun, 1.5/6) {
		t.Errorf("pet.avgCostPerRun: got %f, want %f", pet.AvgCostPerRun, 1.5/6)
	}
	if !nearly(pet.AvgDurationS, 300.0/6) {
		t.Errorf("pet.avgDurationS: got %f, want 50", pet.AvgDurationS)
	}
	if pet.TotalInputTokens != 1000 || pet.TotalOutputTokens != 500 {
		t.Errorf("pet tokens: got in=%d out=%d, want 1000/500", pet.TotalInputTokens, pet.TotalOutputTokens)
	}

	work := find(t, out, "work")
	if work.TotalRuns != 6 || !nearly(work.TotalCostUSD, 3.0) {
		t.Errorf("work bucket wrong: %+v", work)
	}

	unknown := find(t, out, "(unknown)")
	if unknown.TotalRuns != 1 {
		t.Errorf("unknown.totalRuns: got %d, want 1", unknown.TotalRuns)
	}
}

func TestAggregateByProjectType_EmptyMap(t *testing.T) {
	byProject := []stats.GroupedStat{
		{Key: "a/b", Stats: stats.Summary{TotalCostUSD: 1, TotalRuns: 1, TotalDurationS: 10}},
	}
	out := aggregateByProjectType(byProject, map[string]string{})
	if len(out) != 1 || out[0].Key != "(unknown)" {
		t.Fatalf("expected single (unknown) bucket; got %+v", out)
	}
}

func TestAggregateByProjectType_SkipsZeroBuckets(t *testing.T) {
	out := aggregateByProjectType(nil, map[string]string{"a/b": "pet"})
	if len(out) != 0 {
		t.Errorf("empty input → empty output; got %+v", out)
	}
}

func find(t *testing.T, gs []stats.GroupedStat, key string) stats.Summary {
	t.Helper()
	for _, g := range gs {
		if g.Key == key {
			return g.Stats
		}
	}
	t.Fatalf("no bucket with key %q in %+v", key, gs)
	return stats.Summary{}
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
