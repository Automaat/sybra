package agent

import (
	"encoding/json"
	"testing"
)

func TestAddUsage(t *testing.T) {
	got := Usage{
		CostUSD:                  1.5,
		InputTokens:              10,
		OutputTokens:             20,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
		ReasoningTokens:          50,
		PremiumRequests:          2.5,
	}.Add(Usage{
		CostUSD:                  2.25,
		InputTokens:              1,
		OutputTokens:             2,
		CacheCreationInputTokens: 3,
		CacheReadInputTokens:     4,
		ReasoningTokens:          5,
		PremiumRequests:          0.5,
	})

	want := Usage{
		CostUSD:                  3.75,
		InputTokens:              11,
		OutputTokens:             22,
		CacheCreationInputTokens: 33,
		CacheReadInputTokens:     44,
		ReasoningTokens:          55,
		PremiumRequests:          3,
	}
	if got != want {
		t.Fatalf("Usage.Add() = %+v, want %+v", got, want)
	}
}

func TestAgentDoesNotPromoteUsageAdd(t *testing.T) {
	type usageAdder interface {
		Add(Usage) Usage
	}
	if _, ok := any(&Agent{}).(usageAdder); ok {
		t.Fatal("Agent must not expose Usage.Add as a promoted mutator-looking method")
	}
}

func TestAgentUsageSerializesFlat(t *testing.T) {
	a := Agent{
		ID:        "agent-1",
		TaskID:    "task-1",
		Mode:      "headless",
		State:     StateRunning,
		SessionID: "session-1",
		Usage: Usage{
			CostUSD:                  0.42,
			InputTokens:              10,
			OutputTokens:             20,
			CacheCreationInputTokens: 30,
			CacheReadInputTokens:     40,
			ReasoningTokens:          50,
			PremiumRequests:          1.5,
		},
	}

	data, err := json.Marshal(&a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, key := range []string{
		"costUsd",
		"inputTokens",
		"outputTokens",
		"cacheCreationInputTokens",
		"cacheReadInputTokens",
		"reasoningTokens",
		"premiumRequests",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("expected flat usage key %q in %s", key, data)
		}
	}
	for _, key := range []string{"Usage", "usage"} {
		if _, ok := got[key]; ok {
			t.Fatalf("unexpected nested usage key %q in %s", key, data)
		}
	}
}

func TestAgentUsageUnmarshalsFlat(t *testing.T) {
	var got Agent
	data := []byte(`{
		"id": "agent-1",
		"taskId": "task-1",
		"mode": "headless",
		"state": "running",
		"sessionId": "session-1",
		"costUsd": 0.42,
		"inputTokens": 10,
		"outputTokens": 20,
		"cacheCreationInputTokens": 30,
		"cacheReadInputTokens": 40,
		"reasoningTokens": 50,
		"premiumRequests": 1.5
	}`)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	want := Usage{
		CostUSD:                  0.42,
		InputTokens:              10,
		OutputTokens:             20,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
		ReasoningTokens:          50,
		PremiumRequests:          1.5,
	}
	if got.Usage != want {
		t.Fatalf("usage = %+v, want %+v", got.Usage, want)
	}
}

func TestAgentAddUsageStatsAccumulates(t *testing.T) {
	a := &Agent{}

	if got := a.AddResultStats("s1", 0.125, 10, 20, 30); got != 0.125 {
		t.Fatalf("first cost = %v, want 0.125", got)
	}
	if got := a.AddResultStats("s2", 0.25, 1, 2, 3); got != 0.375 {
		t.Fatalf("second cost = %v, want 0.375", got)
	}
	a.AddCacheStats(4, 5)
	a.AddPremiumRequests(1.5)
	a.AddOutputTokens(6)

	want := Usage{
		CostUSD:                  0.375,
		InputTokens:              11,
		OutputTokens:             28,
		CacheCreationInputTokens: 4,
		CacheReadInputTokens:     5,
		ReasoningTokens:          33,
		PremiumRequests:          1.5,
	}
	if a.Usage != want {
		t.Fatalf("usage = %+v, want %+v", a.Usage, want)
	}
	if a.SessionID != "s2" {
		t.Fatalf("SessionID = %q, want s2", a.SessionID)
	}
}

func TestEventUsageAccessors(t *testing.T) {
	want := Usage{
		CostUSD:                  0.25,
		InputTokens:              1,
		OutputTokens:             2,
		CacheCreationInputTokens: 3,
		CacheReadInputTokens:     4,
		ReasoningTokens:          5,
		PremiumRequests:          6.5,
	}

	stream := StreamEvent{
		CostUSD:                  want.CostUSD,
		InputTokens:              want.InputTokens,
		OutputTokens:             want.OutputTokens,
		CacheCreationInputTokens: want.CacheCreationInputTokens,
		CacheReadInputTokens:     want.CacheReadInputTokens,
		ReasoningTokens:          want.ReasoningTokens,
		PremiumRequests:          want.PremiumRequests,
	}
	if got := stream.Usage(); got != want {
		t.Fatalf("StreamEvent.Usage() = %+v, want %+v", got, want)
	}

	convo := ConvoEvent{
		CostUSD:                  want.CostUSD,
		InputTokens:              want.InputTokens,
		OutputTokens:             want.OutputTokens,
		CacheCreationInputTokens: want.CacheCreationInputTokens,
		CacheReadInputTokens:     want.CacheReadInputTokens,
		ReasoningTokens:          want.ReasoningTokens,
		PremiumRequests:          want.PremiumRequests,
	}
	if got := convo.Usage(); got != want {
		t.Fatalf("ConvoEvent.Usage() = %+v, want %+v", got, want)
	}
}
