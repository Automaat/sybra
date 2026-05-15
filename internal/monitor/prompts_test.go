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
	if !strings.Contains(body, "sybra-cli update jkl012 --mode interactive --status todo") {
		t.Error("hint should include interactive-mode option for tasks whose scope exceeds automation")
	}
	if strings.Contains(body, "sybra_bug") {
		t.Error("hint must not reference sybra_bug outcome — that path sets status=blocked, not human-required")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_AfterHumanReview_WithPR(t *testing.T) {
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
			"pr_number":        16601,
		},
	}

	body := DeterministicIssueBody(a)

	if !strings.Contains(body, "PR #16601") {
		t.Error("hint should include PR number when available")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_VerdictHuman(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "abc999",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:abc999",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":              "abc999",
			"status":               "human-required",
			"dwell_h":              10.0,
			"last_agent_role":      "human-review",
			"last_agent_state":     "stopped",
			"human_review_verdict": "human",
		},
	}

	body := DeterministicIssueBody(a)

	if !strings.Contains(body, "confirmed") {
		t.Error("hint should say verdict was confirmed")
	}
	if !strings.Contains(body, "scope beyond automation") {
		t.Error("hint should mention scope beyond automation")
	}
	if !strings.Contains(body, "sybra-cli update abc999 --mode interactive --status todo") {
		t.Error("hint should include interactive hand-off command")
	}
	// When verdict is known-human, the conditional phrasing is no longer needed
	if strings.Contains(body, "If the note says scope exceeds automation") {
		t.Error("hint must not show conditional scope hint when verdict is already known")
	}
	if strings.Contains(body, "Note confirms human input needed") {
		t.Error("hint must not show generic note-check when verdict is already known")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_VerdictHuman_WithPR(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "abc999",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:abc999",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":              "abc999",
			"status":               "human-required",
			"dwell_h":              10.0,
			"last_agent_role":      "human-review",
			"last_agent_state":     "stopped",
			"human_review_verdict": "human",
			"pr_number":            777,
		},
	}

	body := DeterministicIssueBody(a)

	// verdict=human means genuine human input is needed regardless of PR state
	if !strings.Contains(body, "scope") {
		t.Error("hint should mention scope/human-input reason")
	}
	if !strings.Contains(body, "sybra-cli update abc999 --mode interactive --status todo") {
		t.Error("hint should include interactive hand-off command even when PR is linked")
	}
	// Must not redirect to PR merge automation — human input is the blocker, not PR state
	if strings.Contains(body, "APPROVED") {
		t.Error("hint must not suggest merging the PR when verdict=human")
	}
	if strings.Contains(body, "CHANGES_REQUESTED") {
		t.Error("hint must not show PR review state hints when verdict=human")
	}
	if strings.Contains(body, "--status done") {
		t.Error("hint must not tell operator to mark done via PR merge when human input is still needed")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_AfterFixReview(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "pqr678",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:pqr678",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":          "pqr678",
			"status":           "human-required",
			"dwell_h":          10.0,
			"last_agent_role":  "fix-review",
			"last_agent_state": "stopped",
		},
	}

	body := DeterministicIssueBody(a)

	if strings.Contains(body, "Approve or reject the plan") {
		t.Error("human-required hint must not reference plan approval")
	}
	if strings.Contains(body, "Human-review agent") {
		t.Error("hint must not mention human-review when role is fix-review")
	}
	if !strings.Contains(body, "Fix-review agent") {
		t.Error("hint should mention fix-review agent finished")
	}
	if strings.Contains(body, "pushed") {
		t.Error("hint must not claim fixes were pushed — outcome is unknown")
	}
	if !strings.Contains(body, "PR") {
		t.Error("hint should reference PR for fix-review case")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_AfterFixReview_WithPR(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "pqr678",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:pqr678",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":          "pqr678",
			"status":           "human-required",
			"dwell_h":          10.0,
			"last_agent_role":  "fix-review",
			"last_agent_state": "stopped",
			"pr_number":        16601,
		},
	}

	body := DeterministicIssueBody(a)

	if !strings.Contains(body, "PR #16601") {
		t.Error("hint should include PR number when available")
	}
	if !strings.Contains(body, "CHANGES_REQUESTED") {
		t.Error("hint should mention CHANGES_REQUESTED action")
	}
	if !strings.Contains(body, "sybra-cli update pqr678 --status done") {
		t.Error("hint should include done command once PR merges")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_AfterFixReview_WithPR_MergedCase(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "pqr678",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:pqr678",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":          "pqr678",
			"status":           "human-required",
			"dwell_h":          10.0,
			"last_agent_role":  "fix-review",
			"last_agent_state": "stopped",
			"pr_number":        16601,
		},
	}

	body := DeterministicIssueBody(a)

	if !strings.Contains(body, "MERGED") {
		t.Error("hint should mention MERGED case for already-merged PRs")
	}
	if !strings.Contains(body, "sybra-cli update pqr678 --status done") {
		t.Error("hint should include done command for merged PR case")
	}
}

func TestDeterministicIssueBody_StuckHumanBlocked_HumanRequired_FixReviewStillRunning(t *testing.T) {
	a := Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      "stu901",
		Severity:    SeverityWarn,
		Fingerprint: "stuck_human_blocked:stu901",
		DetectedAt:  time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
		Evidence: map[string]any{
			"task_id":          "stu901",
			"status":           "human-required",
			"dwell_h":          3.0,
			"last_agent_role":  "fix-review",
			"last_agent_state": "running",
		},
	}

	body := DeterministicIssueBody(a)

	if strings.Contains(body, "Fix-review agent") {
		t.Error("hint must not claim fix-review completed when agent is still running")
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

	t.Run("includes linked PR when pr_number set", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"pr_number": 16601,
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if !strings.Contains(prompt, "Linked PR: #16601") {
			t.Error("prompt missing Linked PR line")
		}
	})

	t.Run("human-review hint when last agent is human-review stopped", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "human-review",
			"last_agent_state": "stopped",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if !strings.Contains(prompt, "Auto-review verdict") {
			t.Error("prompt should direct agent to the Auto-review verdict section")
		}
		if !strings.Contains(prompt, "awaiting PR reviewer") {
			t.Error("prompt should mention awaiting PR reviewer case")
		}
		if strings.Contains(prompt, "most recent agent log") {
			t.Error("prompt should not ask agent to re-read agent log when human-review verdict exists")
		}
	})

	t.Run("human-review hint distinguishes filing failures from status bug", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "human-review",
			"last_agent_state": "stopped",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		// All three failure-qualified variants must be listed so the agent can
		// recognise a legitimately human-required task (issue filing failed)
		// and not misreport it as a status-transition bug.
		for _, variant := range []string{"no issue payload", "local task creation failed", "issue submission failed"} {
			if !strings.Contains(prompt, variant) {
				t.Errorf("prompt must name filing failure variant %q", variant)
			}
		}
		// Bare sybra_bug (no qualifier) should still be flagged as a status bug.
		if !strings.Contains(prompt, "sybra_bug") {
			t.Error("prompt must still mention sybra_bug for the bare-verdict status-bug case")
		}
	})

	t.Run("no human-review hint when agent still running", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "human-review",
			"last_agent_state": "running",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if strings.Contains(prompt, "Auto-review verdict") {
			t.Error("prompt must not reference Auto-review verdict while human-review is still running")
		}
		if !strings.Contains(prompt, "most recent agent log") {
			t.Error("prompt should use standard investigation hint when agent is still running")
		}
	})

	t.Run("fix-review hint when last agent is fix-review stopped", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "fix-review",
			"last_agent_state": "stopped",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if strings.Contains(prompt, "most recent agent log") {
			t.Error("prompt must not ask agent to re-read agent log when fix-review already ran")
		}
		if !strings.Contains(prompt, "reviewDecision") {
			t.Error("prompt should direct agent to check PR review state")
		}
		if !strings.Contains(prompt, "CHANGES_REQUESTED") {
			t.Error("prompt should mention CHANGES_REQUESTED case")
		}
		if !strings.Contains(prompt, "REVIEW_REQUIRED") {
			t.Error("prompt should mention REVIEW_REQUIRED case")
		}
	})

	t.Run("fix-review hint includes task done command", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "fix-review",
			"last_agent_state": "stopped",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if !strings.Contains(prompt, "sybra-cli update abc --status done") {
			t.Error("prompt should include done command for task id")
		}
	})

	t.Run("fix-review hint includes PR number when available", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "fix-review",
			"last_agent_state": "stopped",
			"pr_number":        16601,
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if !strings.Contains(prompt, "PR #16601") {
			t.Error("prompt should mention specific PR number when available")
		}
	})

	t.Run("fix-review hint includes MERGED case", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "fix-review",
			"last_agent_state": "stopped",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if !strings.Contains(prompt, "state=MERGED") {
			t.Error("prompt should mention MERGED case for fix-review")
		}
		if !strings.Contains(prompt, "state,reviewDecision") {
			t.Error("prompt should include state field in gh pr view command")
		}
	})

	t.Run("no fix-review hint when agent still running", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(map[string]any{
			"last_agent_role":  "fix-review",
			"last_agent_state": "running",
		})}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if strings.Contains(prompt, "reviewDecision") {
			t.Error("prompt must not reference PR review state while fix-review is still running")
		}
		if !strings.Contains(prompt, "most recent agent log") {
			t.Error("prompt should use standard investigation hint when fix-review agent is still running")
		}
	})

	t.Run("omits linked PR when pr_number absent", func(t *testing.T) {
		a := Anomaly{Kind: KindStuckHumanBlocked, Evidence: copyEv(nil)}
		prompt := DispatchPrompt(a, "owner/repo", "")
		if strings.Contains(prompt, "Linked PR:") {
			t.Error("prompt should not contain Linked PR when pr_number absent")
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
