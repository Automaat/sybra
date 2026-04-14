package monitor

import (
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/task"
)

// liveAgent is the minimal projection of agent.Agent the detector needs.
// Defining a tiny shape here keeps detector.go importable from tests without
// pulling internal/agent into the test build.
type liveAgent struct {
	TaskID  string
	Running bool
}

// DetectInput aggregates everything the detector needs in one struct so the
// public Detect signature stays stable as new rules arrive.
type DetectInput struct {
	Now           time.Time
	Tasks         []task.Task
	Events15m     []audit.Event
	HourSummary   audit.Summary
	LiveAgents    []liveAgent
	Cfg           config.MonitorConfig
	AllowsProject func(projectID string) bool
}

// Detect runs every threshold rule against the snapshot and returns a Report
// with Counts populated and Anomalies in deterministic order. It performs no
// I/O so it is safe to call from the CLI and from tests.
func Detect(in DetectInput) Report {
	report := Report{
		GeneratedAt: in.Now,
		Counts:      countByStatus(in.Tasks),
	}
	report.Anomalies = append(report.Anomalies, detectBoardWide(in, report.Counts)...)
	report.Anomalies = append(report.Anomalies, detectPerTask(in)...)
	report.Anomalies = append(report.Anomalies, detectFromAudit(in)...)
	return report
}

func countByStatus(tasks []task.Task) Counts {
	c := Counts{ByStatus: make(map[string]int, len(task.AllStatuses()))}
	for _, st := range task.AllStatuses() {
		c.ByStatus[string(st)] = 0
	}
	for i := range tasks {
		s := string(tasks[i].Status)
		c.ByStatus[s]++
		switch tasks[i].Status {
		case task.StatusNew:
			c.New++
		case task.StatusTodo:
			c.Todo++
		case task.StatusInProgress:
			c.InProgress++
		case task.StatusInReview:
			c.InReview++
		case task.StatusPlanReview:
			c.PlanReview++
		case task.StatusHumanRequired:
			c.HumanRequired++
		case task.StatusDone:
			c.Done++
		case task.StatusPlanning, task.StatusTesting, task.StatusTestPlanReview:
			// Tracked in ByStatus only — not promoted to a top-level counter.
		}
	}
	return c
}

func detectBoardWide(in DetectInput, counts Counts) []Anomaly {
	if counts.InProgress <= in.Cfg.DispatchLimit {
		return nil
	}
	ev := map[string]any{
		"in_progress": counts.InProgress,
		"limit":       in.Cfg.DispatchLimit,
	}
	return []Anomaly{{
		Kind:        KindOverDispatchLimit,
		Severity:    SeverityWarn,
		RequiresLLM: false,
		Fingerprint: Fingerprint(KindOverDispatchLimit, "", ev),
		Evidence:    ev,
		DetectedAt:  in.Now,
	}}
}

func detectPerTask(in DetectInput) []Anomaly {
	var out []Anomaly
	stuckBudget := time.Duration(in.Cfg.StuckHumanHours * float64(time.Hour))
	for i := range in.Tasks {
		t := &in.Tasks[i]
		if t.TaskType == task.TaskTypeChat {
			continue
		}
		if !projectAllowed(in.AllowsProject, t.ProjectID) {
			continue
		}
		if a := detectUntriaged(t, in.Now); a != nil {
			out = append(out, *a)
		}
		if a := detectStuckHumanBlocked(t, in.Now, stuckBudget); a != nil {
			out = append(out, *a)
		}
		if a := detectPRGap(t, in.Now); a != nil {
			out = append(out, *a)
		}
	}
	out = append(out, detectLostAgents(in)...)
	return out
}

func detectUntriaged(t *task.Task, now time.Time) *Anomaly {
	if t.Status != task.StatusTodo {
		return nil
	}
	if len(t.Tags) > 0 && t.AgentMode != "" {
		return nil
	}
	ev := map[string]any{
		"task_id":    t.ID,
		"title":      t.Title,
		"agent_mode": t.AgentMode,
		"tags":       t.Tags,
	}
	return &Anomaly{
		Kind:        KindUntriaged,
		TaskID:      t.ID,
		Severity:    SeverityInfo,
		RequiresLLM: false,
		Fingerprint: Fingerprint(KindUntriaged, t.ID, ev),
		Evidence:    ev,
		DetectedAt:  now,
	}
}

func detectStuckHumanBlocked(t *task.Task, now time.Time, budget time.Duration) *Anomaly {
	if t.Status != task.StatusPlanReview && t.Status != task.StatusHumanRequired {
		return nil
	}
	dwell := now.Sub(t.UpdatedAt)
	if dwell <= budget {
		return nil
	}
	ev := map[string]any{
		"task_id":   t.ID,
		"title":     t.Title,
		"status":    string(t.Status),
		"dwell_h":   dwell.Hours(),
		"budget_h":  budget.Hours(),
		"file_path": t.FilePath,
	}
	return &Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      t.ID,
		Severity:    SeverityWarn,
		RequiresLLM: true,
		Fingerprint: Fingerprint(KindStuckHumanBlocked, t.ID, ev),
		Evidence:    ev,
		DetectedAt:  now,
	}
}

func detectPRGap(t *task.Task, now time.Time) *Anomaly {
	if t.Status != task.StatusInReview {
		return nil
	}
	if t.ProjectID == "" || t.PRNumber > 0 {
		return nil
	}
	ev := map[string]any{
		"task_id":    t.ID,
		"title":      t.Title,
		"project_id": t.ProjectID,
		"branch":     t.Branch,
	}
	return &Anomaly{
		Kind:        KindPRGap,
		TaskID:      t.ID,
		Severity:    SeverityWarn,
		RequiresLLM: true,
		Fingerprint: Fingerprint(KindPRGap, t.ID, ev),
		Evidence:    ev,
		DetectedAt:  now,
	}
}

func detectLostAgents(in DetectInput) []Anomaly {
	if in.Cfg.LostAgentMinutes <= 0 {
		return nil
	}
	live := liveTaskIDs(in.LiveAgents)
	active := tasksWithRecentAgentEvents(in.Events15m)
	var out []Anomaly
	for i := range in.Tasks {
		t := &in.Tasks[i]
		if t.Status != task.StatusInProgress {
			continue
		}
		if t.TaskType == task.TaskTypeChat {
			continue
		}
		if !projectAllowed(in.AllowsProject, t.ProjectID) {
			continue
		}
		if active[t.ID] || live[t.ID] {
			continue
		}
		ev := map[string]any{
			"task_id": t.ID,
			"title":   t.Title,
			"window":  in.Cfg.LostAgentMinutes,
		}
		out = append(out, Anomaly{
			Kind:        KindLostAgent,
			TaskID:      t.ID,
			Severity:    SeverityError,
			RequiresLLM: false,
			Fingerprint: Fingerprint(KindLostAgent, t.ID, ev),
			Evidence:    ev,
			DetectedAt:  in.Now,
		})
	}
	return out
}

func detectFromAudit(in DetectInput) []Anomaly {
	var out []Anomaly
	if in.HourSummary.FailureRate > in.Cfg.FailureRateThreshold && in.HourSummary.AgentRuns > 0 {
		ev := map[string]any{
			"failure_rate": in.HourSummary.FailureRate,
			"agent_runs":   in.HourSummary.AgentRuns,
			"threshold":    in.Cfg.FailureRateThreshold,
			"period":       in.HourSummary.Period,
		}
		out = append(out, Anomaly{
			Kind:        KindFailureSpike,
			Severity:    SeverityError,
			RequiresLLM: true,
			Fingerprint: Fingerprint(KindFailureSpike, "", ev),
			Evidence:    ev,
			DetectedAt:  in.Now,
		})
	}
	for status, dwell := range in.HourSummary.StatusBottlenecks {
		threshold := bottleneckThreshold(in.Cfg, status)
		if dwell <= threshold {
			continue
		}
		ev := map[string]any{
			"status":    status,
			"dwell_h":   dwell,
			"threshold": threshold,
		}
		out = append(out, Anomaly{
			Kind:        KindBottleneck,
			Severity:    SeverityWarn,
			RequiresLLM: true,
			Fingerprint: Fingerprint(KindBottleneck, "", ev),
			Evidence:    ev,
			DetectedAt:  in.Now,
		})
	}
	return out
}

func bottleneckThreshold(cfg config.MonitorConfig, status string) float64 {
	if cfg.BottleneckHours == nil {
		return 12
	}
	if v, ok := cfg.BottleneckHours[status]; ok && v > 0 {
		return v
	}
	if v, ok := cfg.BottleneckHours["default"]; ok && v > 0 {
		return v
	}
	return 12
}

func liveTaskIDs(live []liveAgent) map[string]bool {
	out := make(map[string]bool, len(live))
	for _, a := range live {
		if a.TaskID == "" || !a.Running {
			continue
		}
		out[a.TaskID] = true
	}
	return out
}

func tasksWithRecentAgentEvents(events []audit.Event) map[string]bool {
	out := make(map[string]bool)
	for i := range events {
		e := events[i]
		if !strings.HasPrefix(e.Type, "agent.") {
			continue
		}
		if e.TaskID == "" {
			continue
		}
		out[e.TaskID] = true
	}
	return out
}

func projectAllowed(fn func(string) bool, projectID string) bool {
	if fn == nil || projectID == "" {
		return true
	}
	return fn(projectID)
}
