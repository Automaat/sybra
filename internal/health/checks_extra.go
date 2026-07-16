package health

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

const (
	retryLoopThreshold = 2
)

// statusDwellThresholds maps task status → max acceptable hours in that status
// before flagging a bottleneck. Statuses absent from the map are not checked.
var statusDwellThresholds = map[string]float64{
	"plan-review":    12,
	"in-review":      24,
	"testing":        12,
	"ready-pr":       2, // transient (PR opens in seconds); flag fast if PR-open stalls
	"human-required": 24,
	"todo":           24,
}

// isAgentFailure classifies an audit event as a failed agent run. Production
// emits failures as agent.completed with state != "stopped"; the legacy
// agent.failed type is also accepted for forward/test compatibility.
func isAgentFailure(e audit.Event) bool {
	runs := audit.NormalizeAgentRuns([]audit.Event{e})
	return len(runs) == 1 && runs[0].Terminal && runs[0].Failed
}

// checkAgentRetryLoops flags tasks that have 2+ failed agent runs in the
// window — an indicator that headless retries are not converging and the task
// likely needs mode change, prompt refinement, or human intervention.
func checkAgentRetryLoops(events []audit.Event, now time.Time) []Finding {
	failuresPerTask := make(map[string]int)
	lastRole := make(map[string]string)
	runs := audit.NormalizeAgentRuns(events)
	for i := range runs {
		run := &runs[i]
		if !run.Terminal || !run.Failed || run.TaskID == "" {
			continue
		}
		failuresPerTask[run.TaskID]++
		if role, ok := run.TerminalEvent.Data["role"].(string); ok {
			lastRole[run.TaskID] = role
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
		if to == "human-required" && classifiedHeadless[e.TaskID] {
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
				"final_status":    "human-required",
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
