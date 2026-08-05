package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/task"
)

// auditAPI mirrors the slice of internal/audit the service needs. Tests
// inject a fake; the production wiring uses auditFunc below.
type auditAPI interface {
	Read(q audit.Query) ([]audit.Event, error)
}

// auditFunc adapts audit.Read on a fixed directory to the auditAPI shape.
type auditFunc func(q audit.Query) ([]audit.Event, error)

func (f auditFunc) Read(q audit.Query) ([]audit.Event, error) { return f(q) }

// agentLister is the slice of agent.Manager the service uses for the
// lost_agent live-suppression check. Real wiring passes a thin adapter so the
// tests don't need a real Manager.
type agentLister interface {
	ListAgents() []*agent.Agent
}

// EmitFunc is the signature App passes for Wails events. Defined locally so
// the package can be imported without pulling internal/sybra.
type EmitFunc func(event string, data any)

// Deps groups Service constructor inputs so the wiring at app.go is named
// rather than positional.
type Deps struct {
	Cfg           config.MonitorConfig
	Tasks         taskAPI
	Audit         auditAPI
	Agents        agentLister
	ObserverOnly  bool
	Dispatcher    Dispatcher
	Sink          IssueSink
	Emit          EmitFunc
	Logger        *slog.Logger
	Now           func() time.Time
	AllowsProject func(projectID string) bool
	// DowngradeLLMForTask, when non-nil, is consulted for every anomaly with
	// RequiresLLM=true after detection. Returning true for an anomaly's
	// TaskID forces its RequiresLLM flag to false, routing it through the
	// deterministic-body path instead of the agent-driven path. Used by
	// callers (e.g. internal/sybra) to keep work-typed anomalies away from
	// LLM-generated issue content that is hard to scrub. See CLAUDE.md —
	// Work-Data Confidentiality.
	DowngradeLLMForTask func(taskID string) bool
	// RecoverLostAgent is called after a lost_agent remediation marks stale
	// run state stopped. Production wires this to the canonical owner's
	// recovery path so the monitor detection immediately hands the
	// in-progress task back to workflow/agent recovery on the assigned node.
	RecoverLostAgent func(context.Context, string)
	// FetchPRState checks linked PR terminal state. Production uses GitHub REST
	// so human-required tasks whose PR landed outside the normal review poll
	// loop do not wait for an LLM stuck-task investigation.
	FetchPRState func(repo string, number int) (github.PRState, error)
	// LandClosedPR advances the same landing pipeline the review handler uses
	// when a linked PR is observed closed or merged outside its normal poll.
	LandClosedPR func(context.Context, string, int, string) error
}

// Service runs the monitor loop. It is constructed once at app startup and
// runs until its context is cancelled.
type Service struct {
	cfg                 config.MonitorConfig
	tasks               taskAPI
	audit               auditAPI
	agents              agentLister
	observerOnly        bool
	dispatcher          Dispatcher
	sink                IssueSink
	emit                EmitFunc
	logger              *slog.Logger
	now                 func() time.Time
	allowsProject       func(string) bool
	downgradeLLMForTask func(taskID string) bool
	fetchPRState        func(repo string, number int) (github.PRState, error)
	landClosedPR        func(context.Context, string, int, string) error
	state               *runState
	rem                 *remediator
}

type remediationResult struct {
	labels []string
	// lostAgentCauses maps a lost_agent anomaly's base fingerprint to the
	// remediation error message, for anomalies whose remediation this tick
	// returned an error. fileIssues uses presence in this map to file
	// immediately (an outright remediation failure) rather than waiting for
	// the occurrence-streak gate a merely-recurring-but-"successful"
	// remediation goes through.
	lostAgentCauses map[string]string
}

// NewService validates dependencies and returns a Service ready for Run.
func NewService(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Dispatcher == nil {
		d.Dispatcher = NoopDispatcher()
	}
	if d.Sink == nil {
		d.Sink = NoopSink()
	}
	if d.Emit == nil {
		d.Emit = func(string, any) {}
	}
	if d.FetchPRState == nil {
		d.FetchPRState = github.FetchPRStateViaREST
	}
	return &Service{
		cfg:                 d.Cfg,
		tasks:               d.Tasks,
		audit:               d.Audit,
		agents:              d.Agents,
		observerOnly:        d.ObserverOnly,
		dispatcher:          d.Dispatcher,
		sink:                d.Sink,
		emit:                d.Emit,
		logger:              d.Logger,
		now:                 d.Now,
		allowsProject:       d.AllowsProject,
		downgradeLLMForTask: d.DowngradeLLMForTask,
		fetchPRState:        d.FetchPRState,
		landClosedPR:        d.LandClosedPR,
		state:               newRunState(),
		rem:                 newRemediator(d.Tasks, d.RecoverLostAgent, d.FetchPRState, d.LandClosedPR),
	}
}

// Run blocks until ctx is cancelled, ticking on cfg.IntervalSeconds. It runs
// one tick immediately so the GUI has a fresh report without waiting a full
// interval.
func (s *Service) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("monitor.disabled")
		return
	}
	interval := time.Duration(s.cfg.IntervalSeconds) * time.Second
	if interval < time.Minute {
		interval = 5 * time.Minute
	}
	s.logger.Info("monitor.start", "interval", interval.String(), "observer_only", s.observerOnly)

	s.tickAndLog(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tickAndLog(ctx)
		}
	}
}

func (s *Service) tickAndLog(ctx context.Context) {
	report, err := s.tick(ctx)
	if err != nil {
		s.logger.Warn("monitor.tick.failed", "err", err)
		return
	}
	s.logger.Info(
		"monitor.tick",
		"in_progress", report.Counts.InProgress,
		"todo", report.Counts.Todo,
		"anomalies", len(report.Anomalies),
		"remediated", len(report.Remediated),
		"dispatched", len(report.Dispatched),
		"issues_opened", report.IssuesOpened,
		"issues_updated", report.IssuesUpdated,
		"issues_closed", report.IssuesClosed,
	)
}

// tick is the single canonical pipeline: snapshot → detect → remediate →
// dispatch → file issues → emit. Exported via Scan for read-only callers.
func (s *Service) tick(ctx context.Context) (Report, error) {
	now := s.now()
	tasks, err := s.tasks.List()
	if err != nil {
		return Report{}, err
	}
	preRemediated := []string(nil)
	if !s.observerOnly {
		preRemediated = s.closeMergedHumanRequiredPRs(ctx, tasks)
		if len(preRemediated) > 0 {
			tasks, err = s.tasks.List()
			if err != nil {
				return Report{}, err
			}
		}
	}
	since15 := now.Add(-15 * time.Minute)
	events15, _ := s.audit.Read(audit.Query{Since: since15, Until: now})
	since1h := now.Add(-1 * time.Hour)
	events1h, _ := s.audit.Read(audit.Query{Since: since1h, Until: now})
	summary := audit.Summarize(events1h, since1h, now)

	live := snapshotLiveAgents(s.agents)
	s.logPRGapGraceSuppressions(tasks, now)
	report := Detect(DetectInput{
		Now:           now,
		Tasks:         tasks,
		Events15m:     events15,
		HourSummary:   summary,
		LiveAgents:    live,
		Cfg:           s.cfg,
		AllowsProject: s.allowsProject,
	})
	report.Anomalies = SortAnomalies(report.Anomalies)
	s.applyDowngradeLLM(report.Anomalies)

	if !s.observerOnly {
		rem := s.applyRemediations(ctx, report.Anomalies)
		report.Remediated = make([]string, 0, len(preRemediated)+len(rem.labels))
		report.Remediated = append(report.Remediated, preRemediated...)
		report.Remediated = append(report.Remediated, rem.labels...)
		report.Dispatched = s.dispatchLLMAnomalies(ctx, now, report.Anomalies)
		opened, updated := s.fileIssues(ctx, now, report.Anomalies, rem.lostAgentCauses)
		report.IssuesOpened = opened
		report.IssuesUpdated = updated
		report.IssuesClosed = s.closeRecoveredLostAgents(ctx, report.Anomalies)
		report.IssuesClosed += s.closeRecoveredUntriaged(ctx, tasks, now)
	}

	s.state.recordReport(report, now)
	s.emit(events.MonitorReport, report)
	metrics.MonitorTick(ctx)
	for i := range report.Anomalies {
		metrics.MonitorAnomaly(ctx, string(report.Anomalies[i].Kind))
	}
	return report, nil
}

func (s *Service) closeMergedHumanRequiredPRs(ctx context.Context, tasks []task.Task) []string {
	_ = ctx
	if s.fetchPRState == nil {
		return nil
	}
	var labels []string
	for i := range tasks {
		t := &tasks[i]
		if t.Status != task.StatusHumanRequired || t.ProjectID == "" || t.PRNumber == 0 {
			continue
		}
		if !projectAllowed(s.allowsProject, t.ProjectID) {
			continue
		}
		state, err := s.fetchPRState(t.ProjectID, t.PRNumber)
		if err != nil {
			s.logger.Debug("monitor.human-required-pr-state.failed", "task_id", t.ID, "pr", t.PRNumber, "err", err)
			continue
		}
		if state.State != "MERGED" {
			continue
		}
		if s.landClosedPR != nil {
			if err := s.landClosedPR(ctx, t.ID, t.PRNumber, state.State); err != nil {
				s.logger.Warn("monitor.human-required-pr-merged.land_failed", "task_id", t.ID, "pr", t.PRNumber, "err", err)
				continue
			}
		} else {
			if _, err := s.tasks.ApplyStatusEffect(t.ID, task.StatusEffect{
				Source:         "monitor.close-merged-human-required-pr",
				ToStatus:       task.StatusDone,
				ExpectedStatus: t.Status,
				Extra: task.Update{
					StatusReason: task.Ptr("monitor: linked PR already merged"),
					Outcome:      task.Ptr("merged"),
				},
			}); err != nil {
				s.logger.Warn("monitor.human-required-pr-merged.update_failed", "task_id", t.ID, "pr", t.PRNumber, "err", err)
				continue
			}
		}
		label := "linked_pr_merged:" + t.ID
		labels = append(labels, label)
		s.logger.Info("monitor.human-required-pr-merged", "task_id", t.ID, "pr", t.PRNumber)
	}
	return labels
}

func (s *Service) logPRGapGraceSuppressions(tasks []task.Task, now time.Time) {
	grace := time.Duration(s.cfg.PRGapGraceMinutes) * time.Minute
	if grace <= 0 {
		return
	}
	for i := range tasks {
		t := &tasks[i]
		if t.Status != task.StatusInReview {
			continue
		}
		if t.ProjectID == "" || t.PRNumber > 0 {
			continue
		}
		if !projectAllowed(s.allowsProject, t.ProjectID) {
			continue
		}
		if t.UpdatedAt.IsZero() {
			continue
		}
		dwell := now.Sub(t.UpdatedAt)
		if dwell < 0 || dwell >= grace {
			continue
		}
		s.logger.Debug(
			"monitor.pr_gap.grace_suppressed",
			"task_id", t.ID,
			"updated_at", t.UpdatedAt.Format(time.RFC3339),
			"grace", grace.String(),
			"dwell", dwell.String(),
		)
	}
}

// Scan runs one detector pass with no remediation, dispatch, or issue side
// effects. Used by `sybra-cli monitor scan` and by tests.
func (s *Service) Scan(_ context.Context) (Report, error) {
	now := s.now()
	tasks, err := s.tasks.List()
	if err != nil {
		return Report{}, err
	}
	since15 := now.Add(-15 * time.Minute)
	events15, _ := s.audit.Read(audit.Query{Since: since15, Until: now})
	since1h := now.Add(-1 * time.Hour)
	events1h, _ := s.audit.Read(audit.Query{Since: since1h, Until: now})
	summary := audit.Summarize(events1h, since1h, now)
	live := snapshotLiveAgents(s.agents)
	report := Detect(DetectInput{
		Now:           now,
		Tasks:         tasks,
		Events15m:     events15,
		HourSummary:   summary,
		LiveAgents:    live,
		Cfg:           s.cfg,
		AllowsProject: s.allowsProject,
	})
	report.Anomalies = SortAnomalies(report.Anomalies)
	return report, nil
}

// LastReport returns the most recent finished report and a flag indicating
// whether a tick has completed yet.
func (s *Service) LastReport() (Report, bool) {
	r, _, ok := s.state.snapshot()
	return r, ok
}

// applyDowngradeLLM forces RequiresLLM=false on any anomaly whose TaskID the
// downgrade closure rejects. Used to route work-typed anomalies away from the
// agent-driven filing path (which produces hard-to-scrub LLM output) into the
// deterministic-body path where the issue sink applies redaction. No-op when
// the closure is unset.
func (s *Service) applyDowngradeLLM(anoms []Anomaly) {
	if s.downgradeLLMForTask == nil {
		return
	}
	for i := range anoms {
		if !anoms[i].RequiresLLM || anoms[i].TaskID == "" {
			continue
		}
		if s.downgradeLLMForTask(anoms[i].TaskID) {
			anoms[i].RequiresLLM = false
		}
	}
}

func (s *Service) applyRemediations(ctx context.Context, anoms []Anomaly) remediationResult {
	res := remediationResult{lostAgentCauses: map[string]string{}}
	for i := range anoms {
		a := anoms[i]
		if a.RequiresLLM {
			continue
		}
		if a.Kind != KindLostAgent && a.Kind != KindUntriaged && !isHumanRequiredStuck(a) {
			continue
		}
		label, err := s.rem.Apply(ctx, a)
		if err != nil {
			s.logger.Warn("monitor.remediate.failed", "kind", a.Kind, "task_id", a.TaskID, "err", err)
			if a.Kind == KindLostAgent {
				res.lostAgentCauses[a.Fingerprint] = err.Error()
			}
			continue
		}
		res.labels = append(res.labels, label)
	}
	return res
}

func (s *Service) dispatchLLMAnomalies(ctx context.Context, now time.Time, anoms []Anomaly) []string {
	cooldown := time.Duration(s.cfg.IssueCooldownMinutes) * time.Minute
	var out []string
	for i := range anoms {
		a := anoms[i]
		if !a.RequiresLLM {
			continue
		}
		ok, skipReason := s.dispatcher.Dispatchable(a)
		if !ok {
			if skipReason != "" {
				s.logger.Warn("monitor.dispatch.undispatchable", "kind", a.Kind, "task_id", a.TaskID, "reason", skipReason)
			}
			continue
		}
		if !s.state.canDispatch(a.Fingerprint, now, cooldown) {
			continue
		}
		agentID, err := s.dispatcher.Dispatch(ctx, a)
		if err != nil {
			// A dispatch refused for want of provider capacity never ran, so
			// it must not consume the anomaly's cooldown. Charging the fleet's
			// outage to the task is what leaves an anomaly unexamined for a
			// full cooldown window after every rate limit.
			if errors.Is(err, provider.ErrProviderUnhealthy) {
				s.state.releaseDispatch(a.Fingerprint)
				s.logger.Warn("monitor.dispatch.no-capacity",
					"kind", a.Kind, "fingerprint", a.Fingerprint, "err", err)
				continue
			}
			s.logger.Warn("monitor.dispatch.failed", "kind", a.Kind, "fingerprint", a.Fingerprint, "err", err)
			continue
		}
		out = append(out, string(a.Kind)+":"+agentID)
	}
	return out
}

// fileIssues files deterministic-body issues for anomalies that were neither
// dispatched to an LLM nor fully handled in-process. Returns (opened, updated).
//
// lostAgentCauses carries the remediation failure message for lost_agent
// anomalies whose remediation errored this tick (see applyRemediations); its
// presence is what lets a lost_agent anomaly file immediately instead of
// going through the occurrence-streak gate in gateLostAgentIssue.
func (s *Service) fileIssues(ctx context.Context, now time.Time, anoms []Anomaly, lostAgentCauses map[string]string) (opened, updated int) {
	cooldown := time.Duration(s.cfg.IssueCooldownMinutes) * time.Minute
	for i := range anoms {
		a := anoms[i]
		if a.RequiresLLM || isHumanRequiredStuck(a) {
			continue
		}
		var body string
		if a.Kind == KindLostAgent {
			var ok bool
			if a, body, ok = s.gateLostAgentIssue(a, lostAgentCauses); !ok {
				continue
			}
		} else {
			body = DeterministicIssueBody(a)
		}
		if !s.state.canIssue(a.Fingerprint, now, cooldown) {
			continue
		}
		created, err := s.sink.Submit(ctx, a, body)
		if err != nil {
			if errors.Is(err, ErrGHRateLimit) {
				s.logger.Warn("monitor.issue.rate_limited", "kind", a.Kind, "fingerprint", a.Fingerprint)
				return opened, updated
			}
			s.logger.Warn("monitor.issue.failed", "kind", a.Kind, "fingerprint", a.Fingerprint, "err", err)
			continue
		}
		if a.Kind == KindLostAgent {
			s.state.lostAgentFiled(anoms[i].Fingerprint, a.Fingerprint)
		}
		if created {
			opened++
		} else {
			updated++
		}
	}
	return opened, updated
}

// gateLostAgentIssue decides whether a lost_agent anomaly should reach the
// issue sink this tick, and with what body:
//
//   - An outright remediation failure this tick (recorded in causes) files
//     immediately — remediation has already failed at least once, so there is
//     nothing to wait for. The fingerprint is qualified with the failure cause
//     so a distinct failure mode never collapses into whatever issue an
//     earlier, unrelated cause opened.
//   - Otherwise, remediation "succeeded" (resetLostAgent is best-effort and
//     always returns nil short of a task-store error), so the anomaly only
//     recurring means recovery hasn't taken effect *yet* — a single
//     detection is not "failed", it's business as usual. Filing waits for
//     LostAgentIssueAfterOccurrences consecutive detections; recurrences past
//     the first that actually files post a terse occurrence-count comment
//     instead of the full body, so a self-healing anomaly doesn't read as a
//     stream of fresh incidents.
func (s *Service) gateLostAgentIssue(a Anomaly, causes map[string]string) (Anomaly, string, bool) {
	baseFP := a.Fingerprint
	// Always record the hit, even on the already-filed/immediate-file
	// branches below, so a tracking entry exists for closeRecoveredLostAgents
	// to later clear and auto-close.
	streak := s.state.lostAgentHit(baseFP, a.TaskID)
	if filedFP, alreadyFiled := s.state.lostAgentFiledFP(baseFP); alreadyFiled {
		// An issue is already open for this task's lost_agent condition:
		// keep commenting on it regardless of this tick's cause/streak
		// state, rather than re-evaluating the gate — which could otherwise
		// open a second, differently-fingerprinted issue once a transient
		// remediation error clears while the underlying condition persists.
		a.Fingerprint = filedFP
		return a, RecurrenceComment(a, streak), true
	}
	if cause, failed := causes[baseFP]; failed {
		a.Fingerprint = Fingerprint(a.Kind, a.TaskID, map[string]any{"cause": cause})
		return a, DeterministicIssueBody(a), true
	}
	if streak < s.cfg.LostAgentIssueAfterOccurrences {
		s.logger.Debug("monitor.lost_agent.remediation_pending", "task_id", a.TaskID, "fingerprint", baseFP, "streak", streak)
		return a, "", false
	}
	// At or past threshold and not yet successfully filed: always emit the full
	// diagnostic body. The alreadyFiled check above owns the "issue exists, just
	// comment" case, so a streak that has overshot the threshold (e.g. because a
	// prior submit on the qualifying tick failed transiently) must still produce
	// the full body — not a terse recurrence comment that would become the entire
	// body of the brand-new issue this create actually opens.
	return a, DeterministicIssueBody(a), true
}

// closeRecoveredLostAgents auto-closes any previously-filed lost_agent
// issue/task whose condition has stayed clear for
// LostAgentAutoCloseAfterClears consecutive ticks — the same "stayed clear
// for N scans" intent as the #2433 merged-PR task auto-close, applied here
// to monitor-filed issues rather than tasks. No-op when the sink doesn't
// implement IssueCloser.
func (s *Service) closeRecoveredLostAgents(ctx context.Context, anoms []Anomaly) int {
	seen := make(map[string]bool, len(anoms))
	for i := range anoms {
		if anoms[i].Kind == KindLostAgent {
			seen[anoms[i].Fingerprint] = true
		}
	}
	cleared := s.state.lostAgentSweepClears(seen, s.cfg.LostAgentAutoCloseAfterClears)
	if len(cleared) == 0 {
		return 0
	}
	closer, ok := s.sink.(IssueCloser)
	if !ok {
		// No closer wired: nothing can ever close these, so drop them now
		// rather than re-returning them (with an ever-climbing clearStreak)
		// on every future tick.
		for _, c := range cleared {
			s.state.lostAgentForget(c.fp)
		}
		return 0
	}
	closed := 0
	for _, c := range cleared {
		a := Anomaly{Kind: KindLostAgent, TaskID: c.taskID, Fingerprint: c.filedFP}
		comment := fmt.Sprintf("monitor: condition cleared for %d consecutive scans; auto-closing.", s.cfg.LostAgentAutoCloseAfterClears)
		didClose, err := closer.CloseIfOpen(ctx, a, comment)
		if err != nil {
			// Keep the tracking entry alive (lostAgentSweepClears did not
			// delete it) so the next tick retries this close instead of
			// permanently orphaning the open issue/task.
			s.logger.Warn("monitor.lost_agent.autoclose_failed", "task_id", c.taskID, "fingerprint", c.filedFP, "err", err)
			continue
		}
		// Close succeeded or the issue was already gone: forget the entry so
		// a future recurrence starts a clean streak.
		s.state.lostAgentForget(c.fp)
		if didClose {
			closed++
			s.logger.Info("monitor.lost_agent.autoclosed", "task_id", c.taskID, "fingerprint", c.filedFP)
		}
	}
	return closed
}

// closeRecoveredUntriaged auto-closes any previously-filed untriaged
// investigation whose source task is no longer untriaged — either because it
// was triaged successfully or because it was deleted outright. Unlike
// lost_agent this is resolved on the first clear tick: once a task has mode +
// tags again, the monitor chore is stale immediately.
func (s *Service) closeRecoveredUntriaged(ctx context.Context, tasks []task.Task, now time.Time) int {
	closer, ok := s.sink.(IssueCloser)
	if !ok {
		return 0
	}
	active := make(map[string]bool)
	for i := range tasks {
		t := &tasks[i]
		if detectUntriaged(t, now) != nil {
			active[t.ID] = true
		}
	}
	open := openUntriagedInvestigations(tasks)
	if len(open) == 0 {
		return 0
	}
	closed := 0
	for taskID, fp := range open {
		if active[taskID] {
			continue
		}
		a := Anomaly{Kind: KindUntriaged, TaskID: taskID, Fingerprint: fp}
		didClose, err := closer.CloseIfOpen(ctx, a, "monitor: source task is no longer untriaged; auto-closing.")
		if err != nil {
			s.logger.Warn("monitor.untriaged.autoclose_failed", "task_id", taskID, "fingerprint", fp, "err", err)
			continue
		}
		if didClose {
			closed++
			s.logger.Info("monitor.untriaged.autoclosed", "task_id", taskID, "fingerprint", fp)
		}
	}
	return closed
}

// AuditDirReader builds an auditAPI bound to a fixed directory. Used by the
// production wiring and the CLI command.
func AuditDirReader(dir string) auditAPI {
	return auditFunc(func(q audit.Query) ([]audit.Event, error) {
		return audit.Read(dir, q)
	})
}

func snapshotLiveAgents(src agentLister) []liveAgent {
	if src == nil {
		return nil
	}
	agents := src.ListAgents()
	out := make([]liveAgent, 0, len(agents))
	for _, a := range agents {
		if a == nil {
			continue
		}
		out = append(out, liveAgent{
			TaskID:  a.TaskID,
			Running: a.GetState() == agent.StateRunning,
		})
	}
	return out
}
