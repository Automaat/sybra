package evaluation

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/stats"
)

// EmitFunc emits a Wails event to the frontend.
type EmitFunc func(event string, data any)

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
	Stats      statsReader
	Audit      auditReader
	Emit       EmitFunc
	Logger     *slog.Logger
	Now        func() time.Time
	ReportPath string
}

// Service periodically computes a fleet scorecard from stats + audit data.
// Read-only: it never dispatches agents or files issues.
type Service struct {
	cfg        config.EvaluationConfig
	stats      statsReader
	audit      auditReader
	emit       EmitFunc
	logger     *slog.Logger
	now        func() time.Time
	reportPath string

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
		cfg:        d.Cfg,
		stats:      d.Stats,
		audit:      d.Audit,
		emit:       d.Emit,
		logger:     d.Logger,
		now:        d.Now,
		reportPath: d.ReportPath,
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
	s.logger.Info("evaluation.tick", "landed", rep.Overall.TasksLanded, "autonomy_rate", rep.Overall.AutonomyRate)
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
	return Report{
		GeneratedAt: now,
		Since:       since,
		Until:       now,
		Overall:     Compute(recs, evts, since, now),
		ByProvider:  BreakdownBy(recs, since, now, func(r stats.RunRecord) string { return r.Provider }),
		ByRole:      BreakdownBy(recs, since, now, func(r stats.RunRecord) string { return r.Role }),
		Notes:       deferredNotes,
	}, nil
}

// reportCacheTTL bounds how stale an on-demand report may be. When the ticker
// is disabled, nothing refreshes s.last, so GetEvaluationReport recomputes once
// the cached report ages past this; when enabled, the ticker keeps it fresher.
const reportCacheTTL = 5 * time.Minute

// GetEvaluationReport returns the most recent report, recomputing on demand when
// none exists or the cached one is stale. Bound over HTTP/Wails for the dashboard.
func (s *Service) GetEvaluationReport() Report {
	s.mu.RLock()
	last := s.last
	s.mu.RUnlock()
	if last != nil && s.now().Sub(last.GeneratedAt) < reportCacheTTL {
		return *last
	}
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
	return os.WriteFile(s.reportPath, data, 0o644)
}
