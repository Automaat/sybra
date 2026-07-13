package pressure

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/config"
)

// DefaultSampleIntervalSeconds is used both as the sample cache TTL and the
// deny-log throttle window when config.PressureConfig.SampleIntervalSeconds
// is unset or non-positive.
const DefaultSampleIntervalSeconds = 15

// Gate is a local resource-pressure admission gate consulted before
// dispatching new agent work. A nil *Gate is safe to call: every method
// treats it as "gating disabled" and admits unconditionally — see New.
type Gate struct {
	minDiskFreePct float64
	minMemAvailPct float64
	maxLoadPerCPU  float64
	interval       time.Duration
	probeDir       string
	logger         *slog.Logger
	// sampleFn is swapped out in tests so Admit's threshold/caching logic is
	// exercised without real syscalls.
	sampleFn func(probeDir string) Sample

	mu         sync.Mutex
	cached     Sample
	cachedAt   time.Time
	lastReason string
	lastLogAt  time.Time
}

// New constructs a Gate from cfg. Returns a genuine nil *Gate when
// cfg.Enabled is false — every method on a nil *Gate is safe to call and
// always admits, so callers never need a separate "is gating on" check
// before wiring one in.
func New(cfg config.PressureConfig, probeDir string, logger *slog.Logger) *Gate {
	if !cfg.Enabled {
		return nil
	}
	interval := time.Duration(cfg.SampleIntervalSeconds) * time.Second
	if cfg.SampleIntervalSeconds <= 0 {
		interval = DefaultSampleIntervalSeconds * time.Second
	}
	return &Gate{
		minDiskFreePct: cfg.MinDiskFreePercent,
		minMemAvailPct: cfg.MinMemAvailablePercent,
		maxLoadPerCPU:  cfg.MaxLoadPerCPU,
		interval:       interval,
		probeDir:       probeDir,
		logger:         logger,
		sampleFn:       readSample,
	}
}

// Admit reports whether new agent work should be dispatched right now. A
// false result carries a single-line human-readable reason naming the
// tripped signal, for the caller's status_reason/log. A nil Gate, a sampler
// that returns an all-NaN sample (every signal unreadable), and every
// individual NaN signal all fail OPEN — pressure gating only ever blocks
// dispatch on a signal it could actually read past its configured
// threshold.
func (g *Gate) Admit() (ok bool, reason string) {
	if g == nil {
		return true, ""
	}
	s := g.sample()
	switch {
	case thresholdTripped(s.DiskFreePct, g.minDiskFreePct, true):
		reason = fmt.Sprintf("disk free %.1f%% below minimum %.1f%%", s.DiskFreePct, g.minDiskFreePct)
	case thresholdTripped(s.MemAvailablePct, g.minMemAvailPct, true):
		reason = fmt.Sprintf("memory available %.1f%% below minimum %.1f%%", s.MemAvailablePct, g.minMemAvailPct)
	case thresholdTripped(s.LoadPerCPU, g.maxLoadPerCPU, false):
		reason = fmt.Sprintf("load per cpu %.2f exceeds maximum %.2f", s.LoadPerCPU, g.maxLoadPerCPU)
	default:
		return true, ""
	}
	g.recordDeny(reason)
	return false, reason
}

// Status returns the most recently cached sample and, when the gate is
// currently denying dispatch, the last tripped reason — for the UI/CLI to
// explain a pressure park without forcing a fresh sample. A nil Gate reports
// a zero Sample and no reason.
func (g *Gate) Status() (Sample, string) {
	if g == nil {
		return Sample{}, ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cached, g.lastReason
}

// sample returns the cached Sample, refreshing it when older than interval.
func (g *Gate) sample() Sample {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.cachedAt.IsZero() && time.Since(g.cachedAt) < g.interval {
		return g.cached
	}
	fn := g.sampleFn
	if fn == nil {
		fn = readSample
	}
	g.cached = fn(g.probeDir)
	g.cachedAt = time.Now()
	return g.cached
}

// recordDeny stashes the latest deny reason for Status and logs it, throttled
// to roughly once per interval so a burst of parked dispatch attempts
// against the same underlying condition doesn't flood the log.
func (g *Gate) recordDeny(reason string) {
	g.mu.Lock()
	g.lastReason = reason
	shouldLog := g.lastLogAt.IsZero() || time.Since(g.lastLogAt) >= g.interval
	if shouldLog {
		g.lastLogAt = time.Now()
	}
	g.mu.Unlock()
	if shouldLog && g.logger != nil {
		g.logger.Warn("pressure.gate.deny", "reason", reason)
	}
}
