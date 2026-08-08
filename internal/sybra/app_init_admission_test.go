package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/stats"
)

func TestPreflightUsageAttributesLatestPriorGenerationCohort(t *testing.T) {
	records := []stats.RunRecord{
		{TaskID: "t", TaskGeneration: 2, TaskGenerationKnown: true, CostUSD: 1, InputTokens: 10},
		{TaskID: "t", TaskGeneration: 4, TaskGenerationKnown: true, CostUSD: 2, InputTokens: 20},
		{TaskID: "t", TaskGeneration: 4, TaskGenerationKnown: true, CostUSD: 3, OutputTokens: 30},
		{TaskID: "t", TaskGeneration: 6, TaskGenerationKnown: true, CostUSD: 99, InputTokens: 99},
		{TaskID: "other", TaskGeneration: 4, TaskGenerationKnown: true, CostUSD: 99, InputTokens: 99},
	}
	cost, tokens, runs, known := preflightUsage(records, "t", 5)
	if !known || cost != 5 || tokens != 50 || runs != 2 {
		t.Fatalf("usage = cost:%v tokens:%d runs:%d known:%v", cost, tokens, runs, known)
	}
}

func TestPreflightUsageLeavesLegacyOnlyCohortUnknown(t *testing.T) {
	_, _, runs, known := preflightUsage([]stats.RunRecord{{TaskID: "t", CostUSD: 1}}, "t", 5)
	if known || runs != 0 {
		t.Fatalf("legacy usage was guessed: runs=%d known=%v", runs, known)
	}
}
