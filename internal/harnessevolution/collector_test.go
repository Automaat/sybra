package harnessevolution

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/selfmonitor"
)

func TestEventsFromReport_OnlyActionableVerdicts(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	report := selfmonitor.Report{Findings: []selfmonitor.InvestigatedFinding{
		findingWithVerdict("task-confirmed", selfmonitor.VerdictConfirmed, now),
		findingWithVerdict("task-human", selfmonitor.VerdictNeedsHuman, now),
		findingWithVerdict("task-false", selfmonitor.VerdictFalsePositive, now),
		findingWithVerdict("task-pending", selfmonitor.VerdictPending, now),
	}}

	events := EventsFromReport(report, time.Time{})
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2", len(events))
	}
	got := map[string]bool{}
	for _, ev := range events {
		got[ev.TaskID] = true
	}
	for _, want := range []string{"task-confirmed", "task-human"} {
		if !got[want] {
			t.Fatalf("missing actionable task %q in %#v", want, got)
		}
	}
}

func TestInferFailureKind_PrioritizesSensitiveErrorOverStall(t *testing.T) {
	ls := &selfmonitor.LogSummary{
		StallDetected: true,
		ErrorClasses:  []selfmonitor.ErrorClass{{Class: "permission_denied", Count: 1}},
	}

	got := inferFailureKind(health.Finding{Category: health.CatAgentRetryLoop}, ls)
	if got != "permission_denied" {
		t.Fatalf("failure kind = %q, want permission_denied", got)
	}
}

func TestEventFromFinding_UsesWorkflowTraceIDShape(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	inv := selfmonitor.InvestigatedFinding{
		Finding: health.Finding{
			Category:   health.CatAgentRetryLoop,
			TaskID:     "task-1",
			AgentID:    "agent-1",
			Role:       "implementation",
			Evidence:   map[string]any{"step": "implement"},
			DetectedAt: now,
		},
		Fingerprint: "fp-1",
		Verdict:     selfmonitor.Verdict{Classification: selfmonitor.VerdictConfirmed},
	}

	ev := eventFromFinding(inv)
	if ev.TraceID != stableTraceID("task-1", "implement", "agent-1") {
		t.Fatalf("trace id = %q, want workflow-shaped trace id", ev.TraceID)
	}
}

func findingWithVerdict(taskID, verdict string, detectedAt time.Time) selfmonitor.InvestigatedFinding {
	return selfmonitor.InvestigatedFinding{
		Finding: health.Finding{
			Category:   health.CatAgentRetryLoop,
			TaskID:     taskID,
			AgentID:    "agent-" + taskID,
			Role:       "implementation",
			Evidence:   map[string]any{"step": "implement"},
			DetectedAt: detectedAt,
		},
		Fingerprint: "fp-" + taskID,
		Verdict:     selfmonitor.Verdict{Classification: verdict},
	}
}
