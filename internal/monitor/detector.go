package monitor

import (
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/verdict"
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
	tracked := openLostAgentInvestigations(in.Tasks)
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
		if a := detectStuckHumanBlocked(t, in.Now, stuckBudget, tracked); a != nil {
			out = append(out, *a)
		}
		if a := detectPRGap(t, in.Now, time.Duration(in.Cfg.PRGapGraceMinutes)*time.Minute); a != nil {
			out = append(out, *a)
		}
	}
	out = append(out, detectLostAgents(in)...)
	return out
}

// monitorAutoRetriedTag marks a task the monitor has already auto-retried
// once out of human-required because its stall correlated with a known,
// already-tracked lost_agent investigation. Present so a second stall on the
// same task falls back to the normal LLM re-investigation instead of
// bouncing the task between in-progress and human-required forever.
const monitorAutoRetriedTag = "monitor:auto-retried"

// lostAgentInvestigationTag is the tag monitorRoutingSink stamps on a local
// task it files for a KindLostAgent anomaly (see internal/sybra/monitor_sink.go).
const lostAgentInvestigationTag = "monitor:" + string(KindLostAgent)

// affectedTaskMarkerPrefix precedes the origin task id in a deterministic
// issue body's "## Affected task" section (see DeterministicIssueBody). Used
// to recover which task an investigation task was filed for.
const affectedTaskMarkerPrefix = "## Affected task\n- `"

// openLostAgentInvestigations returns the set of task IDs that already have
// an open (non-terminal) local investigation task tracking a lost_agent
// anomaly for them — i.e. monitorRoutingSink already filed (or re-detected
// into) a task for this exact lost_agent fingerprint, so its root cause is
// already known and tracked. Used to skip re-dispatching an LLM to
// rediscover the same finding when the origin task later shows up stuck in
// human-required.
type openLostAgentInvestigation struct {
	observedAt time.Time
}

func openLostAgentInvestigations(tasks []task.Task) map[string]openLostAgentInvestigation {
	out := make(map[string]openLostAgentInvestigation)
	for i := range tasks {
		t := &tasks[i]
		if task.IsTerminalStatus(t.Status) {
			continue
		}
		if !slices.Contains(t.Tags, lostAgentInvestigationTag) {
			continue
		}
		idx := strings.Index(t.Body, affectedTaskMarkerPrefix)
		if idx < 0 {
			continue
		}
		rest := t.Body[idx+len(affectedTaskMarkerPrefix):]
		originID, _, ok := strings.Cut(rest, "`")
		if !ok || originID == "" {
			continue
		}
		fpMarker := "- Fingerprint: `" + Fingerprint(KindLostAgent, originID, nil) + "`"
		if !strings.Contains(t.Body, fpMarker) {
			continue
		}
		observedAt := t.UpdatedAt
		if observedAt.IsZero() {
			observedAt = t.CreatedAt
		}
		if prev, ok := out[originID]; !ok || prev.observedAt.Before(observedAt) {
			out[originID] = openLostAgentInvestigation{observedAt: observedAt}
		}
	}
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

func detectStuckHumanBlocked(t *task.Task, now time.Time, budget time.Duration, trackedLostAgent map[string]openLostAgentInvestigation) *Anomaly {
	// plan-review is intentionally excluded: a plan awaits the human's approval
	// indefinitely and must never be auto-escalated to human-required. Only tasks
	// already in human-required are flagged when they exceed their dwell budget.
	if t.Status != task.StatusHumanRequired {
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
		if dec := lastHumanReviewVerdict(t.AgentRuns); dec != "" {
			ev["human_review_verdict"] = dec
		}
	}
	// A task whose stall already has an open, monitor-filed lost_agent
	// investigation is a known cause, not an ambiguous one — re-dispatching an
	// LLM here would just rediscover the same finding the investigation task
	// already recorded. Skip the LLM once and let the remediator auto-retry
	// instead (remediateHumanRequiredStuck), guarded by monitorAutoRetriedTag
	// so a second stall on the same task falls through to the normal path
	// rather than bouncing forever.
	knownLostAgentCause := currentLostAgentInvestigation(t, trackedLostAgent) && !slices.Contains(t.Tags, monitorAutoRetriedTag)
	if knownLostAgentCause {
		ev["known_lost_agent_investigation"] = true
	}
	// Skip the LLM when human-review already confirmed verdict=human, or when
	// the stall correlates with a known, already-tracked lost_agent cause;
	// otherwise an LLM investigates whether the block is a real human need or a
	// Sybra misfire.
	requiresLLM := ev["human_review_verdict"] != "human" && !knownLostAgentCause
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

func currentLostAgentInvestigation(t *task.Task, tracked map[string]openLostAgentInvestigation) bool {
	inv, ok := tracked[t.ID]
	if !ok || inv.observedAt.IsZero() {
		return false
	}
	cutoff := t.UpdatedAt
	if started := latestAgentRunStarted(t.AgentRuns); !started.IsZero() {
		cutoff = started
	}
	return !inv.observedAt.Before(cutoff)
}

func latestAgentRunStarted(runs []task.AgentRun) time.Time {
	var latest time.Time
	for i := range runs {
		if runs[i].StartedAt.After(latest) {
			latest = runs[i].StartedAt
		}
	}
	return latest
}

func detectPRGap(t *task.Task, now time.Time, grace time.Duration) *Anomaly {
	if t.Status != task.StatusInReview {
		return nil
	}
	if t.ProjectID == "" || t.PRNumber > 0 {
		return nil
	}
	dwell := time.Duration(0)
	if !t.UpdatedAt.IsZero() {
		dwell = now.Sub(t.UpdatedAt)
	}
	if grace > 0 && !t.UpdatedAt.IsZero() && dwell >= 0 && dwell < grace {
		return nil
	}
	ev := map[string]any{
		"task_id":       t.ID,
		"title":         t.Title,
		"project_id":    t.ProjectID,
		"branch":        t.Branch,
		"updated_at":    t.UpdatedAt.Format(time.RFC3339),
		"dwell_minutes": dwell.Minutes(),
		"grace_minutes": grace.Minutes(),
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
		// Skip if the task itself transitioned into in-progress within the
		// window. Dispatch (worktree prep, agent process spawn) can take
		// longer than one monitor cycle to reach the point where AddRun/the
		// first agent.* audit event lands, so a task can have zero AgentRuns
		// yet still be legitimately mid-dispatch. StatusChangedAt is stamped
		// only on an actual status transition (internal/task Store), unlike
		// UpdatedAt which any unrelated field write (tags, status_reason, an
		// audit sidecar) bumps — keying this off UpdatedAt would mask a truly
		// lost agent for as long as anything kept touching the task. Legacy
		// task files predating the field are backfilled by Store.List on
		// the very read this detector uses (internal/task/store.go), so
		// StatusChangedAt is never permanently zero here in practice.
		if !t.StatusChangedAt.IsZero() && in.Now.Sub(t.StatusChangedAt) < window {
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
		if dec, _, err := verdict.Parse(r.Result); err == nil {
			return dec.Decision
		}
	}
	return ""
}
