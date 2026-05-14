package monitor

import (
	"strings"
	"testing"
	"time"
)

func TestDeterministicIssueBody_StuckHumanBlocked_PlanReview(t *testing.T) {
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
	if !strings.Contains(body, "Approve or reject the plan") {
		t.Error("plan-review hint missing")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "def456",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:def456",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":       "def456",
			"status":        "human-required",
			"dwell_h":       9.0,
			"status_reason": "waiting for API key rotation approval",
		},
	}

	body := DeterministicIssueBody(a)

	if strings.Contains(body, "dispatched agent") {
		t.Error("body must not mention dispatched agent for stuck_human_blocked")
	}
	// human-required should NOT suggest plan approval
	if strings.Contains(body, "Approve or reject the plan") {
		t.Error("human-required hint must not reference plan approval")
	}
	if !strings.Contains(body, "status_reason") {
		t.Error("human-required hint missing status_reason reference")
	}
	if !strings.Contains(body, "waiting for API key rotation approval") {
		t.Error("status_reason content not surfaced in hint")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_NoReason(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "ghi789",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:ghi789",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id": "ghi789",
			"status":  "human-required",
			"dwell_h": 10.0,
		},
	}

	body := DeterministicIssueBody(a)

	if strings.Contains(body, "Approve or reject the plan") {
		t.Error("human-required hint must not reference plan approval")
	}
	if !strings.Contains(body, "status_reason") {
		t.Error("human-required hint missing status_reason reference")
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
