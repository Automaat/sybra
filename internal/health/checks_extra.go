package health

import (
	"fmt"
	"time"

	"github.com/Automaat/synapse/internal/audit"
)

const (
	retryLoopThreshold = 2
)

// statusDwellThresholds maps task status → max acceptable hours in that status
// before flagging a bottleneck. Statuses absent from the map are not checked.
var statusDwellThresholds = map[string]float64{
	"plan-review":      12,
	"in-review":        24,
	"test-plan-review": 12,
	"human-required":   24,
	"todo":             24,
}

// isAgentFailure classifies an audit event as a failed agent run. Production
// emits failures as agent.completed with state != "stopped"; the legacy
// agent.failed type is also accepted for forward/test compatibility.
func isAgentFailure(e audit.Event) bool {
	if e.Type == audit.EventAgentFailed {
		return true
	}
	if e.Type != audit.EventAgentCompleted {
		return false
	}
	state, _ := e.Data["state"].(string)
	return state != "" && state != string(stoppedState)
}

const stoppedState = "stopped"

// checkAgentRetryLoops flags tasks that have 2+ failed agent runs in the
// window — an indicator that headless retries are not converging and the task
// likely needs mode change, prompt refinement, or human intervention.
func checkAgentRetryLoops(events []audit.Event, now time.Time) []Finding {
	failuresPerTask := make(map[string]int)
	lastRole := make(map[string]string)
	for _, e := range events {
		if !isAgentFailure(e) {
			continue
		}
		if e.TaskID == "" {
			continue
		}
		failuresPerTask[e.TaskID]++
		if role, ok := e.Data["role"].(string); ok {
			lastRole[e.TaskID] = role
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
