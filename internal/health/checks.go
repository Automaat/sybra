package health

import (
	"fmt"
	"math"
	"time"

	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/task"
)

// costThresholds per agent role. Empty string key covers implementation agents.
var costThresholds = map[string]float64{
	"eval":   0.50,
	"triage": 0.50,
	"plan":   2.00,
	"":       15.00,
	"pr-fix": 5.00,
}

const (
	dailyCostThreshold  = 200.0
	failureRateLimit    = 0.30
	minRunsForAlert     = 5
	stuckHours          = 6
	loopRejectThreshold = 3
	costDriftPercent    = 0.50
	costDriftMinRuns    = 10
)

// checkFailureRate flags when agent failure rate exceeds 30% with 5+ runs.
func checkFailureRate(events []audit.Event, now time.Time) []Finding {
	var completed, failed int
	for _, e := range events {
		switch e.Type {
		case audit.EventAgentCompleted:
			completed++
		case audit.EventAgentFailed:
			failed++
		}
	}

	total := completed + failed
	if total < minRunsForAlert {
		return nil
	}

	rate := float64(failed) / float64(total)
	if rate <= failureRateLimit {
		return nil
	}

	return []Finding{{
		Category:    CatFailureRate,
		Severity:    SeverityCritical,
		Title:       fmt.Sprintf("agent failure rate %.0f%% (%d/%d)", rate*100, failed, total),
		Description: fmt.Sprintf("%d agents failed out of %d total runs in the last 24h", failed, total),
		Evidence: map[string]any{
			"completed":    completed,
			"failed":       failed,
			"failure_rate": round2(rate),
		},
		DetectedAt: now,
	}}
}

// checkCostOutliers flags individual agent runs exceeding per-role thresholds
// and daily total exceeding $200.
func checkCostOutliers(events []audit.Event, now time.Time) []Finding {
	var findings []Finding
	var dailyTotal float64

	for _, e := range events {
		if e.Type != audit.EventAgentCompleted {
			continue
		}
		cost, _ := e.Data["cost_usd"].(float64)
		role, _ := e.Data["role"].(string)
		dailyTotal += cost

		threshold, ok := costThresholds[role]
		if !ok {
			threshold = costThresholds[""]
		}
		if cost <= threshold {
			continue
		}

		logFile, _ := e.Data["log_file"].(string)
		findings = append(findings, Finding{
			Category:    CatCostOutlier,
			Severity:    SeverityWarning,
			Title:       fmt.Sprintf("%s agent cost $%.2f (threshold $%.2f)", roleLabel(role), cost, threshold),
			Description: fmt.Sprintf("Agent run on task %s cost $%.2f, exceeding the $%.2f threshold for %s agents", e.TaskID, cost, threshold, roleLabel(role)),
			TaskID:      e.TaskID,
			AgentID:     e.AgentID,
			Role:        role,
			LogFile:     logFile,
			Evidence: map[string]any{
				"cost_usd":  cost,
				"threshold": threshold,
				"role":      role,
			},
			DetectedAt: now,
		})
	}

	if dailyTotal > dailyCostThreshold {
		findings = append(findings, Finding{
			Category:    CatCostOutlier,
			Severity:    SeverityCritical,
			Title:       fmt.Sprintf("daily cost $%.2f exceeds $%.0f", dailyTotal, dailyCostThreshold),
			Description: fmt.Sprintf("Total agent spend in the last 24h is $%.2f", dailyTotal),
			Evidence: map[string]any{
				"daily_total": round2(dailyTotal),
				"threshold":   dailyCostThreshold,
			},
			DetectedAt: now,
		})
	}

	return findings
}

// checkStuckTasks flags tasks in-progress for over 6h with no completion event.
func checkStuckTasks(events []audit.Event, tasks []task.Task, now time.Time) []Finding {
	// Build set of tasks that had a completion/failure event.
	resolved := make(map[string]bool)
	for _, e := range events {
		if e.Type == audit.EventAgentCompleted || e.Type == audit.EventAgentFailed {
			resolved[e.TaskID] = true
		}
	}

	var findings []Finding
	for i := range tasks {
		t := &tasks[i]
		if t.Status != task.StatusInProgress {
			continue
		}
		if resolved[t.ID] {
			continue
		}
		stuckDuration := now.Sub(t.UpdatedAt)
		if stuckDuration < stuckHours*time.Hour {
			continue
		}

		findings = append(findings, Finding{
			Category:    CatStuckTask,
			Severity:    SeverityWarning,
			Title:       fmt.Sprintf("task %s stuck in-progress %.1fh", t.ID, stuckDuration.Hours()),
			Description: fmt.Sprintf("Task %q has been in-progress for %.1f hours with no agent completion event", t.Title, stuckDuration.Hours()),
			TaskID:      t.ID,
			Evidence: map[string]any{
				"stuck_hours": round2(stuckDuration.Hours()),
				"updated_at":  t.UpdatedAt.Format(time.RFC3339),
			},
			DetectedAt: now,
		})
	}

	return findings
}

// checkWorkflowLoops flags tasks with 3+ plan rejections (triage→plan→reject loops).
func checkWorkflowLoops(events []audit.Event, now time.Time) []Finding {
	rejectCounts := make(map[string]int)
	for _, e := range events {
		if e.Type == audit.EventPlanRejected {
			rejectCounts[e.TaskID]++
		}
	}

	var findings []Finding
	for taskID, count := range rejectCounts {
		if count < loopRejectThreshold {
			continue
		}
		findings = append(findings, Finding{
			Category:    CatWorkflowLoop,
			Severity:    SeverityWarning,
			Title:       fmt.Sprintf("task %s plan rejected %d times", taskID, count),
			Description: fmt.Sprintf("Task has been through %d plan rejection cycles, indicating a triage→plan→reject loop", count),
			TaskID:      taskID,
			Evidence: map[string]any{
				"rejection_count": count,
			},
			DetectedAt: now,
		})
	}

	return findings
}

// checkStatusBounce flags tasks where the same status transition occurs 2+ times.
func checkStatusBounce(events []audit.Event, now time.Time) []Finding {
	// taskID → "from→to" → count
	transitions := make(map[string]map[string]int)

	for _, e := range events {
		if e.Type != audit.EventTaskStatusChanged {
			continue
		}
		from, _ := e.Data["from"].(string)
		to, _ := e.Data["to"].(string)
		if from == "" || to == "" {
			continue
		}

		key := from + "→" + to
		if transitions[e.TaskID] == nil {
			transitions[e.TaskID] = make(map[string]int)
		}
		transitions[e.TaskID][key]++
	}

	var findings []Finding
	for taskID, trans := range transitions {
		for key, count := range trans {
			if count < 2 {
				continue
			}
			findings = append(findings, Finding{
				Category:    CatStatusBounce,
				Severity:    SeverityWarning,
				Title:       fmt.Sprintf("task %s status bounce: %s (%dx)", taskID, key, count),
				Description: fmt.Sprintf("Status transition %s occurred %d times, indicating the task is bouncing between states", key, count),
				TaskID:      taskID,
				Evidence: map[string]any{
					"transition": key,
					"count":      count,
				},
				DetectedAt: now,
			})
		}
	}

	return findings
}

// checkCostDrift compares today's average agent cost to 7-day rolling average.
// Requires reading a wider audit window (caller provides 7-day events separately).
func checkCostDrift(todayEvents, weekEvents []audit.Event, now time.Time) []Finding {
	todayAvg, todayCount := avgCost(todayEvents)
	weekAvg, weekCount := avgCost(weekEvents)

	if todayCount < minRunsForAlert || weekCount < costDriftMinRuns {
		return nil
	}
	if weekAvg == 0 {
		return nil
	}

	drift := (todayAvg - weekAvg) / weekAvg
	if drift <= costDriftPercent {
		return nil
	}

	return []Finding{{
		Category:    CatCostDrift,
		Severity:    SeverityWarning,
		Title:       fmt.Sprintf("cost drift +%.0f%% ($%.2f today vs $%.2f weekly avg)", drift*100, todayAvg, weekAvg),
		Description: fmt.Sprintf("Today's average agent cost ($%.2f over %d runs) is %.0f%% higher than the 7-day average ($%.2f over %d runs)", todayAvg, todayCount, drift*100, weekAvg, weekCount),
		Evidence: map[string]any{
			"today_avg":  round2(todayAvg),
			"today_runs": todayCount,
			"week_avg":   round2(weekAvg),
			"week_runs":  weekCount,
			"drift_pct":  round2(drift * 100),
		},
		DetectedAt: now,
	}}
}

func avgCost(events []audit.Event) (avg float64, n int) {
	var total float64
	var count int
	for _, e := range events {
		if e.Type != audit.EventAgentCompleted {
			continue
		}
		if cost, ok := e.Data["cost_usd"].(float64); ok {
			total += cost
			count++
		}
	}
	if count == 0 {
		return 0, 0
	}
	return total / float64(count), count
}

func roleLabel(role string) string {
	if role == "" {
		return "implementation"
	}
	return role
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
