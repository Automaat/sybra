package selfmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/health"
	"github.com/Automaat/synapse/internal/task"
)

// minInterval is the smallest tick interval the Run loop will honor. Anything
// below this gets clamped to prevent a misconfigured YAML from pegging the
// CPU at 6×/second when someone writes `interval_hours: 0.0001`.
const minInterval = time.Hour

// ErrNoHealthReport is returned by HealthReader implementations when the
// backing source hasn't produced a report yet — e.g. the health.Checker
// hasn't ticked since startup. Callers should treat it as an expected
// "nothing to do" signal, not a hard error.
var ErrNoHealthReport = errors.New("selfmonitor: no health report available")

// EmitFunc is the Wails event emitter shape. Passed in so the service can
// broadcast selfmonitor:report without importing Wails runtime.
type EmitFunc func(name string, payload any)

// TaskAPI is the slice of task.Manager the service needs. Defining a tiny
// interface lets unit tests inject a fake without a filesystem-backed store.
type TaskAPI interface {
	Get(id string) (task.Task, error)
	List() ([]task.Task, error)
}

// HealthReader abstracts the source of the latest health.Report. The
// production impl is DiskHealthReader (reads the file the Checker persists);
// tests inject in-memory stubs.
type HealthReader interface {
	LatestReport() (*health.Report, error)
}

// DiskHealthReader reads the JSON file health.Checker persists each tick.
// Shared between the in-process service and the CLI so both see the same
// snapshot without coupling to the Checker instance.
type DiskHealthReader struct {
	Path string
}

// LatestReport reads and decodes the health report. Missing file surfaces
// as ErrNoHealthReport so the service can treat it as an expected empty
// state instead of a hard failure.
func (r DiskHealthReader) LatestReport() (*health.Report, error) {
	data, err := os.ReadFile(r.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoHealthReport
		}
		return nil, fmt.Errorf("read health report: %w", err)
	}
	var rep health.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("decode health report: %w", err)
	}
	return &rep, nil
}

// Deps groups service construction inputs. Grouping via a struct keeps the
// call site at app wiring declarative and lets tests inject targeted fakes.
type Deps struct {
	Cfg              config.SelfMonitorConfig
	Tasks            TaskAPI
	Health           HealthReader
	Ledger           *Ledger
	LogsDir          string // used as fallback for FindLogFile when Finding.LogFile is empty
	LastReportPath   string // persisted to this path each tick (disk cache for CLI)
	Emit             EmitFunc
	Logger           *slog.Logger
	Now              func() time.Time
	AllowsProject    func(projectID string) bool
	MaxLogEventsHint int // caps loganalyzer.Analyze; 0 → default
}

// Service is the in-process self-monitor loop. Each tick snapshots the
// latest health.Report, distills per-finding agent logs into a LogSummary
// via the analyzer, applies ledger-based triage (auto-suppress chronic false
// positives), persists the resulting Report to disk, and emits a Wails
// event. Phase C/D will bolt the LLM judge and autonomous actor onto this
// skeleton without reshaping the tick loop.
type Service struct {
	deps  Deps
	state *runState
}

// NewService wires a Service with sensible defaults for any optional Deps.
// The function never panics on nil fields — fakes and partial mocks are
// acceptable for unit tests.
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
		deps:  d,
		state: newRunState(),
	}
}

// Run blocks until ctx is done, executing a tick on start and then every
// cfg.IntervalHours. Returns immediately when cfg.Enabled is false so the
// wiring in app.go can call it unconditionally without gating.
func (s *Service) Run(ctx context.Context) {
	if !s.deps.Cfg.Enabled {
		s.deps.Logger.Info("selfmonitor.disabled")
		return
	}
	interval := max(time.Duration(s.deps.Cfg.IntervalHours*float64(time.Hour)), minInterval)
	s.deps.Logger.Info("selfmonitor.start", "interval", interval)
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

// Scan runs one pipeline pass and returns the Report without persisting or
// emitting. Used by `synapse-cli selfmonitor investigate` and tests.
func (s *Service) Scan(ctx context.Context) (Report, error) {
	return s.tick(ctx)
}

// LastReport returns the most recent in-memory report and whether a tick
// has completed since startup.
func (s *Service) LastReport() (Report, time.Time, bool) {
	return s.state.snapshot()
}

func (s *Service) tickAndLog(ctx context.Context) {
	report, err := s.tick(ctx)
	if err != nil {
		s.deps.Logger.Error("selfmonitor.tick", "err", err)
		return
	}
	s.state.recordReport(report, s.deps.Now())
	if err := s.persistReport(report); err != nil {
		s.deps.Logger.Warn("selfmonitor.persist", "err", err)
	}
	s.deps.Emit(events.SelfMonitorReport, report)
	s.deps.Logger.Info("selfmonitor.tick.done",
		"findings", len(report.Findings),
		"suppressed", report.Suppressed,
		"duration_ms", report.DurationMS,
	)
}

func (s *Service) tick(_ context.Context) (Report, error) {
	start := s.deps.Now()
	report := Report{
		SchemaVersion: ReportSchemaVersion,
		GeneratedAt:   start,
	}

	if s.deps.Health == nil {
		return s.finalize(report, start), nil
	}

	hr, err := s.deps.Health.LatestReport()
	if err != nil {
		if errors.Is(err, ErrNoHealthReport) {
			s.deps.Logger.Info("selfmonitor.no_health_report")
			return s.finalize(report, start), nil
		}
		return report, fmt.Errorf("latest health report: %w", err)
	}
	if hr == nil {
		s.deps.Logger.Info("selfmonitor.no_health_report")
		return s.finalize(report, start), nil
	}

	report.HealthScore = hr.Score
	report.PeriodStart = hr.PeriodStart
	report.PeriodEnd = hr.PeriodEnd

	for i := range hr.Findings {
		inv, skipped := s.investigate(&hr.Findings[i])
		if skipped {
			report.Suppressed++
			continue
		}
		report.Findings = append(report.Findings, inv)
	}

	return s.finalize(report, start), nil
}

// investigate runs the per-finding distillation pipeline. Returns skipped=true
// when the ledger auto-suppresses the fingerprint or the AllowsProject gate
// rejects the task's project.
func (s *Service) investigate(f *health.Finding) (InvestigatedFinding, bool) {
	if !s.projectAllowed(f) {
		return InvestigatedFinding{}, true
	}
	if s.autoSuppressed(f.Fingerprint) {
		s.deps.Logger.Info("selfmonitor.suppress", "fp", f.Fingerprint, "category", string(f.Category))
		return InvestigatedFinding{}, true
	}

	inv := InvestigatedFinding{
		Finding:     *f,
		Fingerprint: f.Fingerprint,
		Verdict:     Verdict{Classification: VerdictPending},
	}
	if path := s.resolveLogFile(f); path != "" {
		summary, err := Analyze(path, s.deps.MaxLogEventsHint)
		if err != nil {
			s.deps.Logger.Warn("selfmonitor.analyze", "path", path, "err", err)
		} else {
			inv.LogSummary = &summary
		}
	}
	return inv, false
}

func (s *Service) projectAllowed(f *health.Finding) bool {
	if s.deps.AllowsProject == nil || s.deps.Tasks == nil || f.TaskID == "" {
		return true
	}
	t, err := s.deps.Tasks.Get(f.TaskID)
	if err != nil {
		return true
	}
	if t.ProjectID == "" {
		return true
	}
	return s.deps.AllowsProject(t.ProjectID)
}

func (s *Service) autoSuppressed(fp string) bool {
	if fp == "" || s.deps.Ledger == nil {
		return false
	}
	days := s.deps.Cfg.SuppressionDays
	threshold := s.deps.Cfg.SuppressionThreshold
	if days <= 0 || threshold <= 0 {
		return false
	}
	window := time.Duration(days) * 24 * time.Hour
	return s.deps.Ledger.ShouldAutoSuppress(fp, window, threshold)
}

// resolveLogFile walks three sources in order: (1) the Finding's own
// LogFile, (2) the task's AgentRuns[].LogFile for the matching agent, and
// (3) a glob on LogsDir/agents/{agentID}-*.ndjson. Returns "" when nothing
// resolves — the analyzer skip path handles that gracefully.
func (s *Service) resolveLogFile(f *health.Finding) string {
	if f.LogFile != "" {
		if _, err := os.Stat(f.LogFile); err == nil {
			return f.LogFile
		}
	}
	if f.TaskID != "" && s.deps.Tasks != nil {
		if t, err := s.deps.Tasks.Get(f.TaskID); err == nil {
			for i := range t.AgentRuns {
				run := &t.AgentRuns[i]
				if run.LogFile == "" {
					continue
				}
				if f.AgentID != "" && run.AgentID != f.AgentID {
					continue
				}
				if _, err := os.Stat(run.LogFile); err == nil {
					return run.LogFile
				}
			}
		}
	}
	if f.AgentID != "" && s.deps.LogsDir != "" {
		if path, err := agent.FindLogFile(s.deps.LogsDir, f.AgentID); err == nil {
			return path
		}
	}
	return ""
}

func (s *Service) finalize(r Report, start time.Time) Report {
	end := s.deps.Now()
	r.DurationMS = end.Sub(start).Milliseconds()
	return r
}

func (s *Service) persistReport(r Report) error {
	path := s.deps.LastReportPath
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}
