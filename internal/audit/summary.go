package audit

import (
	"math"
	"time"
)

type Summary struct {
	Period            string             `json:"period"`
	TasksCreated      int                `json:"tasks_created"`
	TasksCompleted    int                `json:"tasks_completed"`
	AvgCycleTimeHours float64            `json:"avg_cycle_time_hours"`
	TotalCostUSD      float64            `json:"total_cost_usd"`
	AgentRuns         int                `json:"agent_runs"`
	FailureRate       float64            `json:"failure_rate"`
	PlanRejectionRate float64            `json:"plan_rejection_rate"`
	StatusBottlenecks map[string]float64 `json:"status_bottlenecks_hours"`
}

func Summarize(events []Event, since, until time.Time) Summary {
	s := Summary{
		Period:            since.Format(time.DateOnly) + " to " + until.Format(time.DateOnly),
		StatusBottlenecks: make(map[string]float64),
	}

	// Track task creation times for cycle time calculation.
	taskCreated := make(map[string]time.Time)
	// Track status entry times for bottleneck calculation.
	statusEntered := make(map[string]time.Time) // taskID -> entered current status
	// Accumulate time per status.
	statusDuration := make(map[string]float64) // status -> total hours
	statusCount := make(map[string]int)

	var cycleTimes []float64
	var completedAgents, failedAgents int
	var planApproved, planRejected int

	for _, e := range events {
		switch e.Type {
		case EventTaskCreated:
			s.TasksCreated++
			taskCreated[e.TaskID] = e.Timestamp

		case EventTaskStatusChanged:
			from, _ := e.Data["from"].(string)
			to, _ := e.Data["to"].(string)

			// Measure time in previous status.
			if entered, ok := statusEntered[e.TaskID]; ok && from != "" {
				hours := e.Timestamp.Sub(entered).Hours()
				statusDuration[from] += hours
				statusCount[from]++
			}
			statusEntered[e.TaskID] = e.Timestamp

			if to == "done" {
				s.TasksCompleted++
				if created, ok := taskCreated[e.TaskID]; ok {
					cycleTimes = append(cycleTimes, e.Timestamp.Sub(created).Hours())
				}
			}

		case EventAgentStarted:
			s.AgentRuns++

		case EventAgentCompleted:
			completedAgents++
			if cost, ok := e.Data["cost_usd"].(float64); ok {
				s.TotalCostUSD += cost
			}

		case EventAgentFailed:
			failedAgents++
			if cost, ok := e.Data["cost_usd"].(float64); ok {
				s.TotalCostUSD += cost
			}

		case EventTriageCompleted, EventPlanCompleted, EventEvalCompleted:
			if cost, ok := e.Data["cost_usd"].(float64); ok {
				s.TotalCostUSD += cost
			}

		case EventPlanApproved:
			planApproved++

		case EventPlanRejected:
			planRejected++
		}
	}

	if len(cycleTimes) > 0 {
		var sum float64
		for _, ct := range cycleTimes {
			sum += ct
		}
		s.AvgCycleTimeHours = round2(sum / float64(len(cycleTimes)))
	}

	total := completedAgents + failedAgents
	if total > 0 {
		s.FailureRate = round2(float64(failedAgents) / float64(total))
	}

	planTotal := planApproved + planRejected
	if planTotal > 0 {
		s.PlanRejectionRate = round2(float64(planRejected) / float64(planTotal))
	}

	s.TotalCostUSD = round2(s.TotalCostUSD)

	for status, dur := range statusDuration {
		if cnt := statusCount[status]; cnt > 0 {
			s.StatusBottlenecks[status] = round2(dur / float64(cnt))
		}
	}

	return s
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
