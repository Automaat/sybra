package harnessevolution

import (
	"testing"
	"time"
)

func TestClusterEvents_GroupsStableCause(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	events := []FailureEvent{
		{Category: "agent_retry_loop", FailureKind: "step_retry_exhausted", WorkflowStep: "implement", Role: "implementation", TraceID: "t1", OccurredAt: now},
		{Category: "agent_retry_loop", FailureKind: "step_retry_exhausted", WorkflowStep: "implement", Role: "implementation", TraceID: "t2", OccurredAt: now.Add(time.Minute)},
		{Category: "agent_retry_loop", FailureKind: "step_retry_exhausted", WorkflowStep: "testing", Role: "test-runner", TraceID: "t3", OccurredAt: now},
	}

	clusters := ClusterEvents(events, 2)
	if len(clusters) != 1 {
		t.Fatalf("clusters len = %d, want 1", len(clusters))
	}
	if clusters[0].Count != 2 {
		t.Fatalf("cluster count = %d, want 2", clusters[0].Count)
	}
	if clusters[0].AffectedStep != "implement" {
		t.Fatalf("affected step = %q, want implement", clusters[0].AffectedStep)
	}
}

func TestClusterEvents_DropsSingletons(t *testing.T) {
	events := []FailureEvent{
		{Category: "failure_rate", FailureKind: "permission_denied", WorkflowStep: "review", TraceID: "t1", OccurredAt: time.Now()},
	}
	if got := ClusterEvents(events, 2); len(got) != 0 {
		t.Fatalf("clusters len = %d, want 0", len(got))
	}
}
