package monitor

import (
	"maps"
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

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_AfterHumanReview(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "jkl012",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:jkl012",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":          "jkl012",
			"status":           "human-required",
			"dwell_h":          10.0,
			"last_agent_role":  "human-review",
			"last_agent_state": "stopped",
		},
	}

	body := DeterministicIssueBody(a)

	if strings.Contains(body, "Approve or reject the plan") {
		t.Error("human-required hint must not reference plan approval")
	}
	if !strings.Contains(body, "Human-review agent") {
		t.Error("hint should mention human-review agent completed assessment")
	}
	if strings.Contains(body, "sybra-verdict") {
		t.Error("hint must not reference sybra-verdict block — onComplete writes a note section, not a raw JSON block")
	}
	if !strings.Contains(body, "auto-review note") {
		t.Error("hint should reference the auto-review note written to the task body by onComplete")
	}
	if !strings.Contains(body, "sybra-cli update jkl012 --status todo") {
		t.Error("hint should include actionable sybra-cli retry command with task id")
	}
	if strings.Contains(body, "sybra_bug") {
		t.Error("hint must not reference sybra_bug outcome — that path sets status=blocked, not human-required")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_HumanReviewStillRunning(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "mno345",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:mno345",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":          "mno345",
			"status":           "human-required",
			"dwell_h":          3.0,
			"last_agent_role":  "human-review",
			"last_agent_state": "running",
		},
	}

	body := DeterministicIssueBody(a)

	if strings.Contains(body, "Human-review agent") {
		t.Error("hint must not claim review completed when agent is still running")
	}
	if strings.Contains(body, "sybra-verdict") {
		t.Error("hint must not reference sybra-verdict when agent is still running")
	}
	if strings.Contains(body, "sybra-cli update") {
		t.Error("hint must not include retry command when agent is still running")
	}
}

func TestDispatchPrompt_StuckHumanBlocked_ExtraEvidence(t *testing.T) {
	base := map[string]any{
		"task_id":   "abc",
		"title":     "some task",
		"status":    "human-required",
		"dwell_h":   9.0,
		"file_path": "/path/abc.md",
	}
	copyEv := func(extra map[string]any) map[string]any {
		m := make(map[string]any, len(base)+len(extra))
		maps.Copy(m, base)
		maps.Copy(m, extra)
		return m
	}

	t.Run("includes status_reason", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"status_reason": "watchdog: turn limit exceeded",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if !strings.Contains(prompt, "watchdog: turn limit exceeded") {
			t.Error("prompt missing status_reason")
		}
	})

	t.Run("includes last_agent_role_and_state", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "fix-review",
			"last_agent_state": "stopped",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if !strings.Contains(prompt, "fix-review") {
			t.Error("prompt missing last_agent_role")
		}
		if !strings.Contains(prompt, "stopped") {
			t.Error("prompt missing last_agent_state")
		}
	})

	t.Run("includes last_agent_state_without_role", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_state": "running",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if !strings.Contains(prompt, "running") {
			t.Error("prompt missing last_agent_state")
		}
	})

	t.Run("omits extra lines when evidence absent", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(nil)}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if strings.Contains(prompt, "Status reason:") {
			t.Error("prompt should not contain Status reason when absent")
		}
		if strings.Contains(prompt, "Last agent:") {
			t.Error("prompt should not contain Last agent when absent")
		}
	})
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
