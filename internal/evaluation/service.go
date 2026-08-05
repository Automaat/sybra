package evaluation

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/skillattr"
	"github.com/Automaat/sybra/internal/stats"

	"github.com/Automaat/sybra/internal/fsutil"
)

// EmitFunc emits a Wails event to the frontend.
type EmitFunc func(event string, data any)

// SLOReportFunc receives the freshly computed SLOReport at the end of every
// tick — wired to internal/sybra/agentorch.Orchestrator.SetSLOReport so the
// default-off concurrency throttle sees the current error budget without the
// orchestrator needing its own audit/stats readers.
type SLOReportFunc func(SLOReport)

type statsReader interface{ All() []stats.RunRecord }

type auditReader interface {
	Read(audit.Query) ([]audit.Event, error)
}

type auditFunc func(audit.Query) ([]audit.Event, error)

func (f auditFunc) Read(q audit.Query) ([]audit.Event, error) { return f(q) }

// AuditDirReader adapts audit.Read over a directory into an auditReader.
func AuditDirReader(dir string) auditReader {
	return auditFunc(func(q audit.Query) ([]audit.Event, error) { return audit.Read(dir, q) })
}

// Deps are the collaborators for the evaluation service.
type Deps struct {
	Cfg        config.EvaluationConfig
	ABTesting  abtest.Config
	Stats      statsReader
	Audit      auditReader
	Emit       EmitFunc
	Logger     *slog.Logger
	Now        func() time.Time
	ReportPath string
	// OnSLOReport, when set, is invoked at the end of every tick with the
	// freshly computed SLOReport. Optional — nil is a no-op.
	OnSLOReport SLOReportFunc
}

// Service periodically computes a fleet scorecard from stats + audit data.
// Read-only: it never dispatches agents or files issues.
type Service struct {
	cfg         config.EvaluationConfig
	abTesting   abtest.Config
	stats       statsReader
	audit       auditReader
	emit        EmitFunc
	logger      *slog.Logger
	now         func() time.Time
	reportPath  string
	onSLOReport SLOReportFunc

	mu   sync.RWMutex
	last *Report
}

// NewService builds the service, filling zero-value dependencies with safe defaults.
func NewService(d Deps) *Service {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Emit == nil {
		d.Emit = func(string, any) {}
	}
	return &Service{
		cfg:         d.Cfg,
		abTesting:   d.ABTesting,
		stats:       d.Stats,
		audit:       d.Audit,
		emit:        d.Emit,
		logger:      d.Logger,
		now:         d.Now,
		reportPath:  d.ReportPath,
		onSLOReport: d.OnSLOReport,
	}
}

// Run ticks on the configured interval, computing and publishing a report each
// time. No-op when disabled. Returns when ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("evaluation.disabled")
		return
	}
	interval := time.Duration(s.cfg.IntervalHours * float64(time.Hour))
	if interval < time.Hour {
		interval = 24 * time.Hour
	}
	s.logger.Info("evaluation.start", "interval", interval.String(), "window_days", s.windowDays())

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
	rep, err := s.Scan(ctx)
	if err != nil {
		s.logger.Warn("evaluation.tick.failed", "err", err)
		return
	}
	s.mu.Lock()
	s.last = &rep
	s.mu.Unlock()
	if err := s.persist(rep); err != nil {
		s.logger.Warn("evaluation.persist", "err", err)
	}
	s.emit(events.EvaluationReport, rep)
	s.logger.Info("evaluation.tick", "landed", rep.Overall.TasksLanded, "autonomy_rate", rep.Overall.AutonomyRate,
		"slo_compliant", rep.SLO.Compliant, "slo_error_budget", rep.SLO.ErrorBudgetRemaining)
	if s.onSLOReport != nil {
		s.onSLOReport(rep.SLO)
	}
}

// Scan computes a fresh report over the configured window without side effects.
func (s *Service) Scan(_ context.Context) (Report, error) {
	now := s.now()
	wd := s.windowDays()
	since := now.AddDate(0, 0, -wd)

	var evts []audit.Event
	if s.audit != nil {
		// Read signal events (human-required/CI-fix/rework) from an extra window
		// back so an in-window landing whose escalation predates the window is not
		// misclassified as autonomous. Landings themselves are still bounded to
		// [since, now] inside Compute.
		signalsSince := now.AddDate(0, 0, -2*wd)
		var err error
		evts, err = s.audit.Read(audit.Query{Since: signalsSince, Until: now})
		if err != nil {
			return Report{}, err
		}
	}
	var recs []stats.RunRecord
	if s.stats != nil {
		recs = s.stats.All()
	}
	abTesting := s.abTestingSnapshot()
	byVariant := compareVariantsByAttribution(recs, evts, since, now, CompareOptions{
		MinSamples:  abTesting.MinSamplesPerVariant,
		Experiments: abTesting.Experiments,
	}, ComparisonAttributionLatestAuthor)
	byVariantContribution := compareVariantsByAttribution(recs, evts, since, now, CompareOptions{
		MinSamples:  abTesting.MinSamplesPerVariant,
		Experiments: abTesting.Experiments,
	}, ComparisonAttributionAnyContribution)
	overall := Compute(recs, evts, since, now)
	rep := Report{
		SchemaVersion: ScorecardSchemaVersion,
		GeneratedAt:   now,
		Since:         since,
		Until:         now,
		Overall:       overall,
		ByProvider:    BreakdownBy(recs, since, now, func(r stats.RunRecord) string { return r.Provider }),
		ByRole:        BreakdownBy(recs, since, now, func(r stats.RunRecord) string { return r.Role }),
		BySkillExecutionMode: BreakdownBy(recs, since, now, func(r stats.RunRecord) string {
			return skillattr.NormalizeExecutionMode(r.SkillExecutionMode)
		}),
		ByAgentModel:             CompareByLatestAuthor(recs, evts, since, now, 20, agentModelCohortKey),
		ByAgentModelContribution: CompareByContribution(recs, evts, since, now, 20, agentModelCohortKey),
		ByExperimentKind:         GroupByKind(byVariant, byVariantContribution, abTesting.Experiments),
		Notes:                    reportNotes(recs, since, now, overall),
	}
	sloTargets := s.cfg.SLO
	if sloTargets == (config.SLOTargets{}) {
		sloTargets = DefaultSLOTargets()
	}
	rep.SLO = EvaluateSLOs(rep.Overall, ComputeSLOSignals(evts, since, now), sloTargets)
	rep.Weaknesses = Weaknesses(rep, sloTargets)
	return rep, nil
}

// GetEvaluationReport computes a fresh report for the dashboard. Refresh must
// reflect agent runs that just completed; returning the startup tick cache for a
// TTL window makes manual checks and the Evaluation tab look empty/stale.
func (s *Service) GetEvaluationReport() Report {
	rep, err := s.Scan(context.Background())
	if err != nil {
		s.logger.Warn("evaluation.ondemand.failed", "err", err)
		return Report{GeneratedAt: s.now()}
	}
	s.mu.Lock()
	s.last = &rep
	s.mu.Unlock()
	return rep
}

// PhaseReport decomposes the lead time of tasks that landed in the window into
// per-phase durations (planning/implementing/testing/review/…), so callers can
// see where end-to-end time is actually spent. Reads task.status_changed history
// from a wider range than the cohort window so an in-window landing that started
// earlier is fully reconstructed. Read-only.
func (s *Service) PhaseReport(_ context.Context) (PhaseReport, error) {
	now := s.now()
	wd := s.windowDays()
	since := now.AddDate(0, 0, -wd)
	if s.audit == nil {
		return PhaseReport{Since: since, Until: now}, nil
	}
	histSince := now.AddDate(0, 0, -3*wd)
	evts, err := s.audit.Read(audit.Query{Since: histSince, Until: now})
	if err != nil {
		return PhaseReport{}, err
	}
	return ComputePhaseDurations(evts, since, now, 10), nil
}

// AutonomyTrend returns all-time / last-week / last-month autonomy plus a
// week-by-week series. Reads full audit history (not just the scorecard
// window) since "all-time" and week buckets further back than the configured
// window need it. Read-only.
func (s *Service) AutonomyTrend(_ context.Context) (AutonomyTrend, error) {
	now := s.now()
	var evts []audit.Event
	if s.audit != nil {
		var err error
		evts, err = s.audit.Read(audit.Query{Since: autonomyAllTimeAnchor, Until: now})
		if err != nil {
			return AutonomyTrend{}, err
		}
	}
	var recs []stats.RunRecord
	if s.stats != nil {
		recs = s.stats.All()
	}
	return ComputeAutonomyTrend(recs, evts, now, autonomyTrendWeeks), nil
}

// SetABTesting swaps the A/B config used by CompareVariants/GroupByKind on
// the next Scan — wired from internal/routing's WeightApplier so a routing
// overlay tick updates the evaluation report's variant readiness/min-sample
// view without waiting for a process restart. Config.yaml edits made through
// the settings UI do not call this today (see the ab_testing hot-reload gap
// noted in internal/sybra/config_registry.go); it exists for the routing
// push path specifically.
func (s *Service) SetABTesting(cfg abtest.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.abTesting = cfg
}

// abTestingSnapshot reads the current A/B config under s.mu so a concurrent
// SetABTesting cannot race Scan's read of the Experiments slice.
func (s *Service) abTestingSnapshot() abtest.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.abTesting
}

// LastReport returns the cached report and whether one exists.
func (s *Service) LastReport() (Report, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil {
		return Report{}, false
	}
	return *s.last, true
}

func (s *Service) windowDays() int {
	if s.cfg.WindowDays <= 0 {
		return 30
	}
	return s.cfg.WindowDays
}

func (s *Service) persist(r Report) error {
	if s.reportPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.reportPath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteMode(s.reportPath, data, 0o644)
}
