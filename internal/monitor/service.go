package monitor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/metrics"
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
	Dispatcher    Dispatcher
	Sink          IssueSink
	Emit          EmitFunc
	Logger        *slog.Logger
	Now           func() time.Time
	AllowsProject func(projectID string) bool
}

// Service runs the monitor loop. It is constructed once at app startup and
// runs until its context is cancelled.
type Service struct {
	cfg           config.MonitorConfig
	tasks         taskAPI
	audit         auditAPI
	agents        agentLister
	dispatcher    Dispatcher
	sink          IssueSink
	emit          EmitFunc
	logger        *slog.Logger
	now           func() time.Time
	allowsProject func(string) bool
	state         *runState
	rem           *remediator
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
	return &Service{
		cfg:           d.Cfg,
		tasks:         d.Tasks,
		audit:         d.Audit,
		agents:        d.Agents,
		dispatcher:    d.Dispatcher,
		sink:          d.Sink,
		emit:          d.Emit,
		logger:        d.Logger,
		now:           d.Now,
		allowsProject: d.AllowsProject,
		state:         newRunState(),
		rem:           newRemediator(d.Tasks),
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
	s.logger.Info("monitor.start", "interval", interval.String())

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
	s.logger.Info("monitor.tick",
		"in_progress", report.Counts.InProgress,
		"todo", report.Counts.Todo,
		"anomalies", len(report.Anomalies),
		"remediated", len(report.Remediated),
		"dispatched", len(report.Dispatched),
		"issues_opened", report.IssuesOpened,
		"issues_updated", report.IssuesUpdated,
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

	report.Remediated = s.applyRemediations(ctx, report.Anomalies)
	report.Dispatched = s.dispatchLLMAnomalies(ctx, now, report.Anomalies)
	opened, updated := s.fileIssues(ctx, now, report.Anomalies, report.Dispatched)
	report.IssuesOpened = opened
	report.IssuesUpdated = updated

	s.state.recordReport(report, now)
	s.emit(events.MonitorReport, report)
	metrics.MonitorTick()
	for i := range report.Anomalies {
		metrics.MonitorAnomaly(string(report.Anomalies[i].Kind))
	}
	return report, nil
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

func (s *Service) applyRemediations(ctx context.Context, anoms []Anomaly) []string {
	var out []string
	for i := range anoms {
		a := anoms[i]
		if a.RequiresLLM {
			continue
		}
		if a.Kind != KindLostAgent && a.Kind != KindUntriaged {
			continue
		}
		label, err := s.rem.Apply(ctx, a)
		if err != nil {
			s.logger.Warn("monitor.remediate.failed", "kind", a.Kind, "task_id", a.TaskID, "err", err)
			continue
		}
		out = append(out, label)
	}
	return out
}

func (s *Service) dispatchLLMAnomalies(ctx context.Context, now time.Time, anoms []Anomaly) []string {
	cooldown := time.Duration(s.cfg.IssueCooldownMinutes) * time.Minute
	var out []string
	for i := range anoms {
		a := anoms[i]
		if !a.RequiresLLM {
			continue
		}
		if !s.state.canDispatch(a.Fingerprint, now, cooldown) {
			continue
		}
		agentID, err := s.dispatcher.Dispatch(ctx, a)
		if err != nil {
			s.logger.Warn("monitor.dispatch.failed", "kind", a.Kind, "fingerprint", a.Fingerprint, "err", err)
			continue
		}
		out = append(out, string(a.Kind)+":"+agentID)
	}
	return out
}

// fileIssues files deterministic-body issues for anomalies that were NOT
// dispatched to an LLM (the dispatched ones file their own issue from the
// agent prompt). Returns (opened, updated).
func (s *Service) fileIssues(ctx context.Context, now time.Time, anoms []Anomaly, dispatched []string) (opened, updated int) {
	dispatchedKinds := make(map[string]bool, len(dispatched))
	for _, d := range dispatched {
		dispatchedKinds[d] = true
	}
	cooldown := time.Duration(s.cfg.IssueCooldownMinutes) * time.Minute
	for i := range anoms {
		a := anoms[i]
		if a.RequiresLLM {
			continue
		}
		if !s.state.canIssue(a.Fingerprint, now, cooldown) {
			continue
		}
		body := DeterministicIssueBody(a)
		created, err := s.sink.Submit(ctx, a, body)
		if err != nil {
			if errors.Is(err, ErrGHRateLimit) {
				s.logger.Warn("monitor.issue.rate_limited", "kind", a.Kind, "fingerprint", a.Fingerprint)
				return opened, updated
			}
			s.logger.Warn("monitor.issue.failed", "kind", a.Kind, "fingerprint", a.Fingerprint, "err", err)
			continue
		}
		if created {
			opened++
		} else {
			updated++
		}
	}
	return opened, updated
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
