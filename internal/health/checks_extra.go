package health

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/runacct"
	"github.com/Automaat/sybra/internal/runoutcome"
	"github.com/Automaat/sybra/internal/taskstatus"
)

const (
	retryLoopThreshold = 2
)

// statusDwellThresholds maps task status → max acceptable hours in that status
// before flagging a bottleneck. Statuses absent from the map are not checked.
var statusDwellThresholds = map[string]float64{
	string(taskstatus.PlanReview):    12,
	string(taskstatus.InReview):      24,
	string(taskstatus.Testing):       12,
	string(taskstatus.ReadyPR):       2, // transient (PR opens in seconds); flag fast if PR-open stalls
	string(taskstatus.HumanRequired): 24,
	string(taskstatus.Todo):          24,
}

// isAgentFailure classifies an audit event as a failed agent run. Production
// emits failures as agent.completed with state != "stopped"; the legacy
// agent.failed type is also accepted for forward/test compatibility.
func isAgentFailure(e audit.Event) bool {
	counts := runacct.Count(audit.RunRecords([]audit.Event{e}), nil, runacct.CountConfig{})
	return counts.Failures == 1
}

// checkAgentRetryLoops flags tasks that have 2+ failed agent runs in the
// window — an indicator that headless retries are not converging and the task
// likely needs mode change, prompt refinement, or human intervention.
func checkAgentRetryLoops(events []audit.Event, now time.Time) []Finding {
	failuresPerTask := make(map[string]int)
	lastRole := make(map[string]string)
	records := audit.RunRecords(events)
	for i := range records {
		rec := records[i]
		if rec.TaskID == "" || rec.Outcome != runoutcome.Failed {
			continue
		}
		failuresPerTask[rec.TaskID]++
		if rec.Role != "" {
			lastRole[rec.TaskID] = rec.Role
		}
	}

	var findings []Finding
	for taskID, count := range failuresPerTask {
		if count < retryLoopThreshold {
			continue
		}
		findings = append(findings, Finding{
			Category:    CatAgentRetryLoop,
			Severity:    SeverityCritical,
			Title:       fmt.Sprintf("task %s has %d failed agent runs", taskID, count),
			Description: fmt.Sprintf("Task %s accumulated %d failed agent runs — retries are not converging, consider mode change or human review", taskID, count),
			TaskID:      taskID,
			Role:        lastRole[taskID],
			Evidence: map[string]any{
				"failure_count": count,
			},
			DetectedAt: now,
		})
	}
	return findings
}

// checkTriageMismatch flags tasks that triage classified as headless but later
// transitioned to human-required — the triage policy under-specified the task.
func checkTriageMismatch(events []audit.Event, now time.Time) []Finding {
	classifiedHeadless := make(map[string]bool)
	for _, e := range events {
		if e.Type != audit.EventTriageClassified {
			continue
		}
		mode, _ := e.Data["mode"].(string)
		if mode == "headless" {
			classifiedHeadless[e.TaskID] = true
		}
	}

	escalated := make(map[string]bool)
	for _, e := range events {
		if e.Type != audit.EventTaskStatusChanged {
			continue
		}
		to, _ := e.Data["to"].(string)
		if to == string(taskstatus.HumanRequired) && classifiedHeadless[e.TaskID] {
			if isExpectedHumanRequired(e) {
				continue
			}
			escalated[e.TaskID] = true
		}
	}

	var findings []Finding
	for taskID := range escalated {
		findings = append(findings, Finding{
			Category:    CatTriageMismatch,
			Severity:    SeverityWarning,
			Title:       fmt.Sprintf("task %s triaged headless but escalated to human-required", taskID),
			Description: fmt.Sprintf("Task %s was classified as headless by triage but later required human intervention — triage rules may be under-specifying complexity", taskID),
			TaskID:      taskID,
			Evidence: map[string]any{
				"classified_mode": "headless",
				"final_status":    string(taskstatus.HumanRequired),
			},
			DetectedAt: now,
		})
	}

	return findings
}

// checkGHIssueAuthFailure flags GitHub issue filing (monitor anomalies,
// human-review) failing on authentication instead of succeeding or failing
// for an unrelated reason — the durable outbox (monitor.DurableGHIssueSink)
// already queues and bounded-retries these, but an operator should still see
// one actionable signal that credentials need attention, not just a log
// line. One finding aggregates every affected sink so a simultaneous
// monitor+human-review outage does not spam the board with duplicates.
func checkGHIssueAuthFailure(events []audit.Event, now time.Time) []Finding {
	sinks := make(map[string]bool)
	var lastErr string
	count := 0
	for _, e := range events {
		if e.Type != audit.EventGHIssueAuthFailed {
			continue
		}
		count++
		if sink, ok := e.Data["sink"].(string); ok && sink != "" {
			sinks[sink] = true
		}
		if errStr, ok := e.Data["err"].(string); ok && errStr != "" {
			lastErr = errStr
		}
	}
	if count == 0 {
		return nil
	}
	sinkNames := make([]string, 0, len(sinks))
	for s := range sinks {
		sinkNames = append(sinkNames, s)
	}
	sort.Strings(sinkNames)

	return []Finding{{
		Category:    CatGHAuthFailure,
		Severity:    SeverityWarning,
		Title:       fmt.Sprintf("GitHub issue filing failed authentication (%s)", strings.Join(sinkNames, ", ")),
		Description: fmt.Sprintf("GitHub issue filing hit an authentication failure %d time(s) across %s in the last 24h — configure github.app or run `gh auth login`. Failed filings are queued in the durable outbox and retried automatically once credentials recover.", count, strings.Join(sinkNames, ", ")),
		Evidence: map[string]any{
			"sinks":      sinkNames,
			"count":      count,
			"last_error": lastErr,
		},
		DetectedAt: now,
	}}
}

// checkGHPushAuthFailure flags git push credential preflight failures
// (project.PreflightPushCredentials, see internal/audit.EventGHPushAuthFailed).
// Unlike GH issue filing, the push path has no durable outbox — a failed
// preflight parks its task in human-required immediately — so this is the
// only host-level signal beyond a per-task status_reason string. Severity is
// critical (not warning, unlike checkGHIssueAuthFailure) because a blocked
// push halts the task outright rather than degrading to a queued retry.
func checkGHPushAuthFailure(events []audit.Event, now time.Time) []Finding {
	var lastErr string
	count := 0
	for _, e := range events {
		if e.Type != audit.EventGHPushAuthFailed {
			continue
		}
		count++
		if errStr, ok := e.Data["err"].(string); ok && errStr != "" {
			lastErr = errStr
		}
	}
	if count == 0 {
		return nil
	}

	return []Finding{{
		Category:    CatGHPushAuthFailure,
		Severity:    SeverityCritical,
		Title:       "git push credential preflight is failing",
		Description: fmt.Sprintf("Git push credential preflight failed %d time(s) in the last 24h, parking task(s) in human-required — configure github.app or run `gh auth login` on the host.", count),
		Evidence: map[string]any{
			"count":      count,
			"last_error": lastErr,
		},
		DetectedAt: now,
	}}
}

// ghAuthStateMisconfigured mirrors github.AuthMisconfigured
// (internal/github/authhealth.go) by value. This package intentionally
// doesn't import internal/github — health analyzes state handed to it, it
// doesn't couple to service internals — so the string is duplicated here
// rather than referencing the typed constant.
const ghAuthStateMisconfigured = "misconfigured"

// checkGHAuthUnavailable turns a proactive gh-auth probe result into a
// finding — the periodic counterpart to checkGHPushAuthFailure/
// checkGHIssueAuthFailure, which only fire after a real push or issue-filing
// attempt has already failed. authenticated is the live result of
// github.Authenticated(), sampled once per health tick (see
// Checker.ghAuthProbe); a nil probe (the default) means this check never
// runs. state is the paired github.AuthHealthSnapshot().State, used to tell
// a permanent credential misconfiguration — which needs a human to rotate
// credentials and gets a single actionable critical finding — apart from a
// transient mint/network failure that the circuit breaker and force-refresh
// are already retrying on their own, which is downgraded to a warning so it
// doesn't page for something expected to self-heal. See #2453.
func checkGHAuthUnavailable(authenticated bool, state string, now time.Time) []Finding {
	if authenticated {
		return nil
	}
	f := Finding{
		Category: CatGHAuthUnavailable,
		Evidence: map[string]any{
			"probe": "gh api rate_limit",
			"state": state,
		},
		DetectedAt: now,
	}
	if state == ghAuthStateMisconfigured {
		f.Severity = SeverityCritical
		f.Title = "GitHub credentials are misconfigured"
		f.Description = "Periodic gh auth probe failed with a permanent credential problem (revoked key, suspended App, removed installation) that will not resolve by retrying. Rotate credentials: reconfigure github.app or run `gh auth login` on the host."
	} else {
		f.Severity = SeverityWarning
		f.Title = "GitHub credentials are temporarily unavailable"
		f.Description = "Periodic gh auth probe failed with a transient error (network blip, GitHub outage, or a force-refresh in flight) — the circuit breaker is suppressing repeat calls and will retry on its own backoff. No action needed unless this persists."
	}
	return []Finding{f}
}

func isExpectedHumanRequired(e audit.Event) bool {
	kind, _ := e.Data["human_kind"].(string)
	return kind == "review_manual" || kind == "review_draft"
}

// checkStatusBottleneck flags statuses where the average dwell time exceeds a
// per-status threshold. Dwell is measured between consecutive status changes
// for the same task; tasks still in a status at window end are not counted.
func checkStatusBottleneck(events []audit.Event, now time.Time) []Finding {
	type stat struct {
		totalHours float64
		count      int
	}
	stats := make(map[string]*stat)
	enteredAt := make(map[string]time.Time)

	for _, e := range events {
		if e.Type != audit.EventTaskStatusChanged {
			continue
		}
		from, _ := e.Data["from"].(string)
		to, _ := e.Data["to"].(string)

		if from != "" {
			if entered, ok := enteredAt[e.TaskID]; ok {
				hours := e.Timestamp.Sub(entered).Hours()
				if stats[from] == nil {
					stats[from] = &stat{}
				}
				stats[from].totalHours += hours
				stats[from].count++
			}
		}
		if to != "" {
			enteredAt[e.TaskID] = e.Timestamp
		}
	}

	var findings []Finding
	for status, threshold := range statusDwellThresholds {
		s := stats[status]
		if s == nil || s.count == 0 {
			continue
		}
		avg := s.totalHours / float64(s.count)
		if avg <= threshold {
			continue
		}
		findings = append(findings, Finding{
			Category:    CatStatusBottleneck,
			Severity:    SeverityWarning,
			Title:       fmt.Sprintf("status %s avg dwell %.1fh (threshold %.0fh)", status, avg, threshold),
			Description: fmt.Sprintf("Tasks spent an average of %.1f hours in %s over %d transitions, exceeding the %.0fh threshold", avg, status, s.count, threshold),
			Evidence: map[string]any{
				"status":          status,
				"avg_hours":       round2(avg),
				"transitions":     s.count,
				"threshold_hours": threshold,
			},
			DetectedAt: now,
		})
	}
	return findings
}
