package learning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/provider"
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

// auditWriter is the subset of *audit.Logger the digest service needs to
// record its own generation/failure events.
type auditWriter interface {
	Log(audit.Event) error
}

// Deps are the collaborators for the Learning Digest service.
type Deps struct {
	Cfg       config.LearningDigestConfig
	ABTesting abtest.Config
	Stats     statsReader
	Audit     auditReader
	AuditLog  auditWriter
	Store     *Store
	// Blocklist returns the current fleet-wide work-project redaction
	// terms (see internal/sybra.App.fleetWorkBlocklist). Called fresh on
	// every generation since registered projects can change over time. An
	// error fails the generation closed — RunNow never persists a digest
	// it could not scrub.
	Blocklist func() ([]string, error)
	Emit      EmitFunc
	Logger    *slog.Logger
	Now       func() time.Time
	// Gate reports provider health/rate-limit state. Non-claude entries are
	// masked unhealthy regardless of Gate's own view — see claudeOnlyGate.
	Gate provider.HealthGate
	// Summarizer runs the digest prompt against a provider. Defaults to
	// llmexec.RunJSON; overridable in tests to exercise the parse/validate/
	// scrub/persist path without shelling out to a real provider CLI.
	Summarizer func(ctx context.Context, prompt string, opts llmexec.Options) (llmexec.Result, error)
}

// Service periodically distills the evaluation scorecard, stats, audit
// signals, and active A/B experiments into a Learning Digest. Generation is
// synchronous and single-flight: a manual RunNow and the ticker never run
// concurrently. A failed or malformed run leaves the previous digest intact
// and records an actionable audit failure — it never marks any task failed.
type Service struct {
	cfg       config.LearningDigestConfig
	abTesting abtest.Config
	stats     statsReader
	audit     auditReader
	auditLog  auditWriter
	store     *Store
	blocklist func() ([]string, error)
	emit      EmitFunc
	logger    *slog.Logger
	now       func() time.Time
	gate      provider.HealthGate
	runJSON   func(ctx context.Context, prompt string, opts llmexec.Options) (llmexec.Result, error)

	genMu sync.Mutex // single-flight guard around generation

	statusMu  sync.RWMutex
	lastRunAt time.Time
}

// NewService builds the service, filling zero-value dependencies with safe
// defaults, mirroring evaluation.NewService.
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
	if d.Blocklist == nil {
		d.Blocklist = func() ([]string, error) { return nil, nil }
	}
	if d.Summarizer == nil {
		d.Summarizer = llmexec.RunJSON
	}
	return &Service{
		cfg:       d.Cfg,
		abTesting: d.ABTesting,
		stats:     d.Stats,
		audit:     d.Audit,
		auditLog:  d.AuditLog,
		store:     d.Store,
		blocklist: d.Blocklist,
		emit:      d.Emit,
		logger:    d.Logger,
		now:       d.Now,
		gate:      d.Gate,
		runJSON:   d.Summarizer,
	}
}

// Run ticks on the configured interval, generating a digest each time enough
// fresh data exists. No-op when disabled. Returns when ctx is cancelled. The
// service is built even when disabled so RunNow/Status still work on demand
// (mirroring evaluation.Service).
func (s *Service) Run(ctx context.Context) {
	if !s.cfg.Enabled {
		s.logger.Info("learning.digest.disabled")
		return
	}
	interval := s.interval()
	s.logger.Info("learning.digest.start", "interval", interval.String(), "window_days", s.windowDays())

	s.tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Service) tick(ctx context.Context) {
	if _, err := s.RunNow(ctx); err != nil {
		s.logger.Warn("learning.digest.tick.failed", "err", err)
	}
}

// RunNow synchronously generates and persists a fresh digest, or returns a
// clear error: another generation already in progress, insufficient fresh
// data in the window, a summarizer/parse/validation failure, or a persist
// error. Every non-single-flight failure records an
// audit.EventLearningDigestFailed with an actionable reason and leaves the
// previously-stored digest untouched.
func (s *Service) RunNow(ctx context.Context) (Digest, error) {
	if !s.genMu.TryLock() {
		return Digest{}, errors.New("learning: digest generation already in progress")
	}
	defer s.genMu.Unlock()

	start := s.now()
	defer func() {
		s.statusMu.Lock()
		s.lastRunAt = start
		s.statusMu.Unlock()
	}()

	prev := s.previous()
	if prev != nil && !prev.Until.IsZero() {
		if elapsed := start.Sub(prev.Until); elapsed < s.interval() {
			reason := fmt.Sprintf("only %s elapsed since last digest (need >= %s)", elapsed.Round(time.Second), s.interval())
			s.recordFailure(start, reason)
			return Digest{}, fmt.Errorf("learning: %s", reason)
		}
	}
	since, until := windowFor(start, prev, s.windowDays(), s.maxWindowDays())

	recs := s.allRuns()
	evts, err := s.readAudit(since, until)
	if err != nil {
		s.recordFailure(start, fmt.Sprintf("audit read failed: %v", err))
		return Digest{}, fmt.Errorf("learning: read audit: %w", err)
	}

	fresh, landed := freshSignal(recs, evts, since, until)
	if fresh < s.cfg.MinRuns || landed < s.cfg.MinLandings {
		reason := fmt.Sprintf("insufficient fresh data in window: %d runs (need >=%d), %d landings (need >=%d)",
			fresh, s.cfg.MinRuns, landed, s.cfg.MinLandings)
		s.recordFailure(start, reason)
		return Digest{}, fmt.Errorf("learning: %s", reason)
	}

	pkt := buildPacket(recs, evts, s.abTesting, since, until, prev)
	prompt := buildPrompt(pkt)

	res, err := s.callSummarizer(ctx, prompt)
	if err != nil {
		s.recordFailure(start, fmt.Sprintf("summarizer failed: %v", err))
		return Digest{}, fmt.Errorf("learning: summarizer: %w", err)
	}

	rd, err := parseDigestJSON(res.Text)
	if err != nil {
		s.recordFailure(start, fmt.Sprintf("malformed digest output: %v", err))
		return Digest{}, fmt.Errorf("learning: %w", err)
	}

	d, err := validateDigest(rd, pkt)
	if err != nil {
		s.recordFailure(start, fmt.Sprintf("digest validation failed: %v", err))
		return Digest{}, fmt.Errorf("learning: %w", err)
	}

	bl, err := s.blocklist()
	if err != nil {
		s.recordFailure(start, fmt.Sprintf("blocklist unavailable: %v", err))
		return Digest{}, fmt.Errorf("learning: blocklist: %w", err)
	}

	d.SchemaVersion = SchemaVersion
	d.GeneratedAt = s.now()
	d.ReportDigest = pkt.ReportDigest
	d.AuthorProvider = res.Provider
	d.AuthorModel = s.cfg.Model
	d.Scrub(bl)

	stored, err := s.store.Put(d)
	if err != nil {
		s.recordFailure(start, fmt.Sprintf("persist failed: %v", err))
		return Digest{}, fmt.Errorf("learning: persist: %w", err)
	}
	if stored {
		s.emit(events.LearningSummary, d)
	}
	s.recordSuccess(start, res, stored)
	return d, nil
}

// Status summarizes the service's current state for the dashboard: whether
// it is enabled, the most recently persisted digest, and an estimate of the
// next scheduled run.
type Status struct {
	Enabled bool      `json:"enabled"`
	Last    *Digest   `json:"last,omitempty"`
	NextRun time.Time `json:"nextRun"`
}

// Status returns the service's current state without generating anything.
// NextRun is anchored to the last persisted digest's Until, not to
// lastRunAt: the ticker's "elapsed" gate in RunNow compares against
// prev.Until, and lastRunAt also moves on manual/failed RunNow attempts, so
// deriving NextRun from it can report a time later than when the next tick
// will actually succeed.
func (s *Service) Status() Status {
	st := Status{Enabled: s.cfg.Enabled}
	prev := s.previous()
	if prev != nil {
		st.Last = prev
	}
	if prev != nil && !prev.Until.IsZero() {
		st.NextRun = prev.Until.Add(s.interval())
		return st
	}
	s.statusMu.RLock()
	last := s.lastRunAt
	s.statusMu.RUnlock()
	if !last.IsZero() {
		st.NextRun = last.Add(s.interval())
	}
	return st
}

func (s *Service) previous() *Digest {
	if s.store == nil {
		return nil
	}
	d, ok, err := s.store.Latest()
	if err != nil || !ok {
		return nil
	}
	return &d
}

func (s *Service) allRuns() []stats.RunRecord {
	if s.stats == nil {
		return nil
	}
	return s.stats.All()
}

func (s *Service) readAudit(since, until time.Time) ([]audit.Event, error) {
	if s.audit == nil {
		return nil, nil
	}
	return s.audit.Read(audit.Query{Since: since, Until: until})
}

// callSummarizer runs the digest prompt through llmexec, pinned to claude
// only: the digest is a read-only aggregate summary with no mutation
// powers, and claude is the only provider this feature has been validated
// against. claudeOnlyGate masks every other provider unhealthy so
// llmexec.RunJSON's fallback loop never reaches them. DisableTools keeps
// that "no mutation powers" claim true even though the prompt embeds the
// previous digest's model-authored NextBets text — a prompt-injected
// instruction in that text has no tool to act through.
func (s *Service) callSummarizer(ctx context.Context, prompt string) (llmexec.Result, error) {
	res, err := s.runJSON(ctx, prompt, llmexec.Options{
		Provider:     "claude",
		Models:       map[string]string{"claude": s.cfg.Model},
		DisableTools: true,
		Logger:       s.logger,
		Gate:         claudeOnlyGate{base: s.gate},
	})
	if err != nil {
		return llmexec.Result{}, err
	}
	if res.Provider != "claude" {
		return llmexec.Result{}, fmt.Errorf("expected claude provider, got %q", res.Provider)
	}
	return res, nil
}

// claudeOnlyGate wraps an optional base provider.HealthGate and reports
// every non-claude provider unhealthy, regardless of the base gate's own
// view — the digest summarizer must never fail over to codex/copilot.
type claudeOnlyGate struct {
	base provider.HealthGate
}

func (g claudeOnlyGate) IsHealthy(p string) bool {
	if p != "claude" {
		return false
	}
	if g.base == nil {
		return true
	}
	return g.base.IsHealthy(p)
}

func (g claudeOnlyGate) RateLimited(p string) bool {
	if p != "claude" {
		return false
	}
	return g.base != nil && g.base.RateLimited(p)
}

func (g claudeOnlyGate) Failover(string) string { return "" }

func (g claudeOnlyGate) Reason(p string) string {
	if g.base == nil {
		return ""
	}
	return g.base.Reason(p)
}

func (g claudeOnlyGate) ReportAuthFailure(p, reason string) {
	if g.base != nil && p == "claude" {
		g.base.ReportAuthFailure(p, reason)
	}
}

func (g claudeOnlyGate) ReportRateLimit(p string, retryAfter time.Duration, reason string) {
	if g.base != nil && p == "claude" {
		g.base.ReportRateLimit(p, retryAfter, reason)
	}
}

func (s *Service) recordSuccess(start time.Time, res llmexec.Result, stored bool) {
	if s.auditLog == nil {
		return
	}
	_ = s.auditLog.Log(audit.Event{
		Timestamp: start,
		Type:      audit.EventLearningDigest,
		Data: map[string]any{
			"provider":    res.Provider,
			"model":       s.cfg.Model,
			"cost_usd":    res.CostUSD,
			"duration_ms": s.now().Sub(start).Milliseconds(),
			"stored":      stored,
		},
	})
}

func (s *Service) recordFailure(start time.Time, reason string) {
	s.logger.Warn("learning.digest.failed", "reason", reason)
	if s.auditLog == nil {
		return
	}
	_ = s.auditLog.Log(audit.Event{
		Timestamp: start,
		Type:      audit.EventLearningDigestFailed,
		Data: map[string]any{
			"reason":      reason,
			"duration_ms": s.now().Sub(start).Milliseconds(),
		},
	})
}

func (s *Service) windowDays() int {
	if s.cfg.WindowDays <= 0 {
		return 7
	}
	return s.cfg.WindowDays
}

func (s *Service) maxWindowDays() int {
	if s.cfg.MaxWindowDays <= 0 {
		return 30
	}
	return s.cfg.MaxWindowDays
}

func (s *Service) interval() time.Duration {
	interval := time.Duration(s.cfg.IntervalHours * float64(time.Hour))
	if interval < time.Hour {
		interval = 24 * time.Hour
	}
	return interval
}
