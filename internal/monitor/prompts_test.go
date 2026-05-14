package monitor

import (
	"strings"
	"testing"
	"time"
)

func TestDeterministicIssueBody_StuckHumanBlocked(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "abc123",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:abc123",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id": "abc123",
			"status":  "plan-review",
			"dwell_h": 8.5,
		},
	}

	body := DeterministicIssueBody(a)

	if !strings.Contains(body, "stuck_human_blocked") {
		t.Error("body missing kind")
	}
	if !strings.Contains(body, "abc123") {
		t.Error("body missing task id")
	}
	// Must NOT reference "dispatched agent's issue comment" — for work-typed
	// downgraded anomalies no agent is dispatched and no GH issue is filed.
	if strings.Contains(body, "dispatched agent") {
		t.Error("body must not mention dispatched agent for stuck_human_blocked")
	}
	if !strings.Contains(body, "approve or reject the plan") {
		t.Error("body missing actionable review guidance")
	}
}

func TestDeterministicIssueBody_DefaultFallback(t *testing.T) {
	a := Anomaly{
		Kind:        KindFailureSpike,
		Fingerprint: "failure_spike",
		DetectedAt:  time.Now(),
	}

	body := DeterministicIssueBody(a)

	if !strings.Contains(body, "dispatched agent") {
		t.Error("non-stuck kinds should still reference the dispatched agent")
	}
}
