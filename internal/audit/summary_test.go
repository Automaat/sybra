package audit

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC)

	events := []Event{
		{Timestamp: base, Type: EventTaskCreated, TaskID: "t1"},
		{Timestamp: base.Add(time.Minute), Type: EventTaskStatusChanged, TaskID: "t1", Data: map[string]any{"from": "new", "to": "todo"}},
		{Timestamp: base.Add(2 * time.Minute), Type: EventAgentStarted, TaskID: "t1", AgentID: "a1"},
		{Timestamp: base.Add(3 * time.Minute), Type: EventTaskStatusChanged, TaskID: "t1", Data: map[string]any{"from": "todo", "to": "in-progress"}},
		{Timestamp: base.Add(time.Hour), Type: EventAgentCompleted, TaskID: "t1", AgentID: "a1", Data: map[string]any{"cost_usd": 0.15, "duration_s": 3420.0}},
		{Timestamp: base.Add(time.Hour + time.Minute), Type: EventTaskStatusChanged, TaskID: "t1", Data: map[string]any{"from": "in-progress", "to": "done"}},

		{Timestamp: base, Type: EventTaskCreated, TaskID: "t2"},
		{Timestamp: base.Add(time.Minute), Type: EventAgentStarted, TaskID: "t2", AgentID: "a2"},
		{Timestamp: base.Add(5 * time.Minute), Type: EventAgentFailed, TaskID: "t2", AgentID: "a2", Data: map[string]any{"cost_usd": 0.05}},

		{Timestamp: base.Add(10 * time.Minute), Type: EventPlanApproved, TaskID: "t3"},
		{Timestamp: base.Add(11 * time.Minute), Type: EventPlanRejected, TaskID: "t4"},
		{Timestamp: base.Add(12 * time.Minute), Type: EventPlanRejected, TaskID: "t5"},
	}

	s := Summarize(events, base.Add(-time.Hour), base.Add(2*time.Hour))

	if s.TasksCreated != 2 {
		t.Errorf("TasksCreated = %d, want 2", s.TasksCreated)
	}
	if s.TasksCompleted != 1 {
		t.Errorf("TasksCompleted = %d, want 1", s.TasksCompleted)
	}
	if s.AgentRuns != 2 {
		t.Errorf("AgentRuns = %d, want 2", s.AgentRuns)
	}
	if s.TotalCostUSD != 0.2 {
		t.Errorf("TotalCostUSD = %f, want 0.2", s.TotalCostUSD)
	}
	if s.FailureRate != 0.5 {
		t.Errorf("FailureRate = %f, want 0.5", s.FailureRate)
	}
	if s.PlanRejectionRate < 0.66 || s.PlanRejectionRate > 0.67 {
		t.Errorf("PlanRejectionRate = %f, want ~0.67", s.PlanRejectionRate)
	}
	if s.AvgCycleTimeHours == 0 {
		t.Error("AvgCycleTimeHours should be > 0")
	}
}

func TestSummarizeEmpty(t *testing.T) {
	t.Parallel()
	s := Summarize(nil, time.Now().Add(-time.Hour), time.Now())
	if s.TasksCreated != 0 || s.TotalCostUSD != 0 {
		t.Error("empty summary should have zero values")
	}
}
