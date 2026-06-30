package monitor

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
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
		case task.StatusPlanning, task.StatusReadyReview, task.StatusTesting,
			task.StatusReadyPR, task.StatusBlocked, task.StatusCancelled:
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
		if a := detectPRGap(t, in.Now, time.Duration(in.Cfg.PRGapGraceMinutes)*time.Minute); a != nil {
			out = append(out, *a)
		}
	}
	out = append(out, detectLostAgents(in)...)
	return out
}

// untriagedGracePeriod is how long a freshly created task is left alone before
// it can be flagged as untriaged. A new task.created workflow (triage) needs a
// moment to pick the task up and assign tags/mode; flagging immediately
// produces spurious "untriaged" anomalies — and, for work-typed tasks, a
// scrubbed sybra-bug — for perfectly normal new tasks.
const untriagedGracePeriod = 15 * time.Minute

func detectUntriaged(t *task.Task, now time.Time) *Anomaly {
	if t.Status != task.StatusTodo {
		return nil
	}
	if len(t.Tags) > 0 && t.AgentMode != "" {
		return nil
	}
	if !t.CreatedAt.IsZero() && now.Sub(t.CreatedAt) < untriagedGracePeriod {
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
	if t.StatusReason != "" {
		ev["status_reason"] = t.StatusReason
	}
	if t.PRNumber > 0 {
		ev["pr_number"] = t.PRNumber
	}
	if n := len(t.AgentRuns); n > 0 {
		last := t.AgentRuns[n-1]
		if last.Role != "" {
			ev["last_agent_role"] = last.Role
		}
		ev["last_agent_state"] = last.State
		// Scan all runs newest-to-oldest for the first parseable human-review
		// verdict. Checking only the last run misses earlier successful verdicts
		// when the most recent human-review run failed (e.g. API 529 Overloaded)
		// and left an empty result with no sybra-verdict block.
		if verdict := lastHumanReviewVerdict(t.AgentRuns); verdict != "" {
			ev["human_review_verdict"] = verdict
		}
	}
	// plan-review is always deterministic: human must approve or reject the plan.
	// For human-required, skip LLM only when human-review confirmed verdict=human.
	requiresLLM := t.Status != task.StatusPlanReview && ev["human_review_verdict"] != "human"
	return &Anomaly{
		Kind:        KindStuckHumanBlocked,
		TaskID:      t.ID,
		Severity:    SeverityWarn,
		RequiresLLM: requiresLLM,
		Fingerprint: Fingerprint(KindStuckHumanBlocked, t.ID, ev),
		Evidence:    ev,
		DetectedAt:  now,
	}
}

func detectPRGap(t *task.Task, now time.Time, grace time.Duration) *Anomaly {
	if t.Status != task.StatusInReview {
		return nil
	}
	if t.ProjectID == "" || t.PRNumber > 0 {
		return nil
	}
	if prGapWithinGrace(t, now, grace) {
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

func prGapWithinGrace(t *task.Task, now time.Time, grace time.Duration) bool {
	if grace <= 0 {
		return false
	}
	if t.UpdatedAt.IsZero() {
		return false
	}
	dwell := now.Sub(t.UpdatedAt)
	return dwell >= 0 && dwell < grace
}

func detectLostAgents(in DetectInput) []Anomaly {
	if in.Cfg.LostAgentMinutes <= 0 {
		return nil
	}
	window := time.Duration(in.Cfg.LostAgentMinutes) * time.Minute
	live := liveTaskIDs(in.LiveAgents)
	active := tasksWithRecentAgentEvents(in.Events15m)
	var out []Anomaly
	for i := range in.Tasks {
		t := &in.Tasks[i]
		if t.Status != task.StatusInProgress {
			continue
		}
		// Chat sessions and umbrella trackers run no agent of their own: a
		// chat is synthetic and an umbrella's in-progress is a rollup of its
		// children (app_umbrella_gate.go). Flagging either as a lost agent would
		// produce noisy recovery handoff reports every cycle. Mirrors recovery's
		// RestartStaleInProgress.
		if t.TaskType == task.TaskTypeChat || t.TaskType == task.TaskTypeUmbrella {
			continue
		}
		if !projectAllowed(in.AllowsProject, t.ProjectID) {
			continue
		}
		if active[t.ID] || live[t.ID] {
			continue
		}
		// Skip if any running agent run started within the window. A freshly
		// dispatched agent may not yet appear in the live list or have written
		// its first audit event (e.g. during app restart recovery), so we wait
		// at least one full window before declaring it lost.
		if recentRunningRun(t.AgentRuns, in.Now, window) {
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

// recentRunningRun reports whether any agent run in state "running" started
// within d of now.
func recentRunningRun(runs []task.AgentRun, now time.Time, d time.Duration) bool {
	for i := range runs {
		if runs[i].State == "running" && now.Sub(runs[i].StartedAt) < d {
			return true
		}
	}
	return false
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

// lastHumanReviewVerdict returns the verdict from the most recent stopped
// human-review run that produced a parseable sybra-verdict. It scans backward
// so that a later failed/overloaded run (empty or unparseable result) does not
// mask a successful verdict from an earlier run.
func lastHumanReviewVerdict(runs []task.AgentRun) string {
	for i := range slices.Backward(runs) {
		r := &runs[i]
		if r.Role != "human-review" {
			continue
		}
		// A running human-review means a verdict is in flight; block any older verdict.
		if r.State != "stopped" {
			return ""
		}
		if r.Verdict != "" {
			return r.Verdict
		}
		if v := parseHumanReviewDecision(r.Result); v != "" {
			return v
		}
	}
	return ""
}

var humanReviewVerdictRe = regexp.MustCompile("(?s)```\\s*sybra-verdict\\s*\\n(.*?)\\n```")

// parseHumanReviewDecision extracts the "decision" field from a sybra-verdict
// fenced block embedded in the human-review agent result text. Returns "human"
// or "sybra_bug" on success, empty string on any parse failure.
func parseHumanReviewDecision(result string) string {
	m := humanReviewVerdictRe.FindStringSubmatch(result)
	if len(m) < 2 {
		return ""
	}
	var v struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v); err != nil {
		return ""
	}
	d := strings.ToLower(strings.TrimSpace(v.Decision))
	if d != "human" && d != "sybra_bug" {
		return ""
	}
	return d
}
