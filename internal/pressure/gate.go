package pressure

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/config"
)

// DefaultSampleIntervalSeconds is used both as the sample cache TTL and the
// deny-log throttle window when config.PressureConfig.SampleIntervalSeconds
// is unset or non-positive.
const DefaultSampleIntervalSeconds = 15

// Gate is a local resource-pressure admission gate consulted before
// dispatching new agent work. A nil *Gate is safe to call: every method treats
// it as "gating disabled" and admits unconditionally. Call sites may still keep
// explicit nil guards when they need enabled-only side effects such as audit.
type Gate struct {
	minDiskFreePct     float64
	minMemAvailPct     float64
	maxLoadPerCPU      float64
	warningDiskFreePct float64
	interval           time.Duration
	probeDir           string
	logger             *slog.Logger
	// sampleFn is swapped out in tests so Admit's threshold/caching logic is
	// exercised without real syscalls.
	sampleFn func(probeDir string) (Sample, error)

	mu             sync.Mutex
	cached         Sample
	cachedAt       time.Time
	lastReason     string
	lastLogAt      time.Time
	reclaimTrigger func()
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
		minDiskFreePct:     cfg.MinDiskFreePercent,
		minMemAvailPct:     cfg.MinMemAvailablePercent,
		maxLoadPerCPU:      cfg.MaxLoadPerCPU,
		warningDiskFreePct: cfg.WarningDiskFreePercent,
		interval:           interval,
		probeDir:           probeDir,
		logger:             logger,
		sampleFn: func(probeDir string) (Sample, error) {
			return readSample(probeDir), nil
		},
	}
}

// SetReclaimTrigger wires in a callback invoked whenever a sample crosses
// WarningDiskFreePercent — i.e. before disk free space reaches
// MinDiskFreePercent and dispatch starts being denied for disk pressure. The
// callback must not block: it is expected to own its own rate limiting and
// dedup (see internal/diskreclaim.Reclaimer.TryRun, the intended caller) so
// Admit stays cheap on every dispatch tick. Optional — nil (the default,
// and the state after calling on a nil *Gate) disables the warning-watermark
// trigger entirely, e.g. in tests or when the feature is off.
func (g *Gate) SetReclaimTrigger(fn func()) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.reclaimTrigger = fn
	g.mu.Unlock()
}

// Status returns the most recently sampled resource-pressure snapshot — the
// same cached value Admit consults — for telemetry callers (e.g. health
// reporting) that need current readings without affecting the deny-log
// throttle. Every field is NaN on a nil Gate.
func (g *Gate) Status() Sample {
	if g == nil {
		return Sample{DiskFreePct: math.NaN(), MemAvailablePct: math.NaN(), LoadPerCPU: math.NaN()}
	}
	s, _ := g.sample()
	return s
}

// Thresholds returns the configured warning and critical (MinDiskFreePercent)
// disk-free-percent thresholds, for telemetry callers. Both are zero on a
// nil Gate.
func (g *Gate) Thresholds() (warningDiskFreePct, criticalDiskFreePct float64) {
	if g == nil {
		return 0, 0
	}
	return g.warningDiskFreePct, g.minDiskFreePct
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
	s, err := g.sample()
	if err != nil {
		if !errors.Is(err, errAllSignalsUnreadable) && g.logger != nil {
			g.logger.Warn("pressure.gate.sample", "err", err)
		}
		g.clearReason()
		return true, ""
	}
	g.maybeTriggerReclaim(s)
	switch {
	case thresholdTripped(s.DiskFreePct, g.minDiskFreePct, true):
		reason = fmt.Sprintf("disk free %.1f%% below minimum %.1f%%", s.DiskFreePct, g.minDiskFreePct)
	case thresholdTripped(s.MemAvailablePct, g.minMemAvailPct, true):
		reason = fmt.Sprintf("memory available %.1f%% below minimum %.1f%%", s.MemAvailablePct, g.minMemAvailPct)
	case thresholdTripped(s.LoadPerCPU, g.maxLoadPerCPU, false):
		reason = fmt.Sprintf("load per cpu %.2f exceeds maximum %.2f", s.LoadPerCPU, g.maxLoadPerCPU)
	default:
		g.clearReason()
		return true, ""
	}
	g.recordDeny(reason)
	return false, reason
}

// maybeTriggerReclaim fires the reclaim trigger (see SetReclaimTrigger) when
// disk free space has crossed WarningDiskFreePercent. Checked before the
// critical-threshold switch in Admit so cleanup gets a chance to run before
// dispatch is ever denied for disk pressure — and still fires even once the
// critical threshold has also been crossed, since a low-but-shrinking disk
// benefits from cleanup at every dispatch tick, not just the first one below
// the warning line.
func (g *Gate) maybeTriggerReclaim(s Sample) {
	g.mu.Lock()
	trigger := g.reclaimTrigger
	g.mu.Unlock()
	if trigger == nil {
		return
	}
	if !thresholdTripped(s.DiskFreePct, g.warningDiskFreePct, true) {
		return
	}
	trigger()
}

var errAllSignalsUnreadable = errors.New("all resource-pressure signals unreadable")

// sample returns the cached Sample, refreshing it when older than interval.
func (g *Gate) sample() (Sample, error) {
	g.mu.Lock()
	if !g.cachedAt.IsZero() && time.Since(g.cachedAt) < g.interval {
		sample := g.cached
		g.mu.Unlock()
		if allSignalsUnreadable(sample) {
			return sample, errAllSignalsUnreadable
		}
		return sample, nil
	}
	fn := g.sampleFn
	probeDir := g.probeDir
	g.mu.Unlock()

	if fn == nil {
		fn = func(probeDir string) (Sample, error) {
			return readSample(probeDir), nil
		}
	}
	sample, err := fn(probeDir)
	if err != nil {
		return Sample{}, err
	}

	g.mu.Lock()
	g.cached = sample
	g.cachedAt = time.Now()
	g.mu.Unlock()
	if allSignalsUnreadable(sample) {
		return sample, errAllSignalsUnreadable
	}
	return sample, nil
}

func allSignalsUnreadable(sample Sample) bool {
	return math.IsNaN(sample.DiskFreePct) &&
		math.IsNaN(sample.MemAvailablePct) &&
		math.IsNaN(sample.LoadPerCPU)
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
		g.logger.Warn("pressure.gate.deferred", "reason", reason)
	}
}

func (g *Gate) clearReason() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastReason = ""
}
