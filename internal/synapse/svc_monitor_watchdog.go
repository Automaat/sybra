package synapse

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/metrics"
)

// Default timing for the monitor watchdog. Heartbeat is written by
// /synapse-monitor on each cycle (every ~5 min). StaleAfter allows two
// missed cycles plus a slack window before we declare the loop dead.
const (
	monitorWatchdogTickInterval = 1 * time.Minute
	monitorWatchdogStaleAfter   = 12 * time.Minute
	monitorWatchdogRecoveryCD   = 10 * time.Minute
)

// MonitorStatus is the snapshot surfaced to the frontend and to Wails
// callers. It is value-typed so it can be returned from a bound method
// and serialized cleanly.
type MonitorStatus struct {
	HeartbeatFile string    `json:"heartbeatFile"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
	AgeSeconds    int64     `json:"ageSeconds"`
	Stale         bool      `json:"stale"`
	Present       bool      `json:"present"`
}

// MonitorWatchdog stats the heartbeat file on a ticker, tracks whether the
// /synapse-monitor loop is alive, emits state transitions, and triggers
// recovery (orchestrator restart) when the loop goes stale. Recovery is
// rate-limited by monitorWatchdogRecoveryCD to avoid thrash.
type MonitorWatchdog struct {
	path         string
	staleAfter   time.Duration
	tickInterval time.Duration
	recoveryCD   time.Duration
	logger       *slog.Logger
	emit         func(string, any)
	recoverFn    func()
	now          func() time.Time

	mu             sync.RWMutex
	status         MonitorStatus
	lastRecoveryAt time.Time
}

// NewMonitorWatchdog wires a watchdog with the default thresholds. The
// recoverFn callback is invoked while the heartbeat is stale (rate-limited
// by the cooldown) and should force the /synapse-monitor cron to be
// re-created on this machine — typically by restarting the orchestrator
// session.
func NewMonitorWatchdog(
	heartbeatPath string,
	logger *slog.Logger,
	emit func(string, any),
	recoverFn func(),
) *MonitorWatchdog {
	return &MonitorWatchdog{
		path:         heartbeatPath,
		staleAfter:   monitorWatchdogStaleAfter,
		tickInterval: monitorWatchdogTickInterval,
		recoveryCD:   monitorWatchdogRecoveryCD,
		logger:       logger,
		emit:         emit,
		recoverFn:    recoverFn,
		now:          time.Now,
		status:       MonitorStatus{HeartbeatFile: heartbeatPath},
	}
}

// Run blocks until ctx is done, polling the heartbeat file every
// tickInterval. It runs one check immediately so the frontend has fresh
// data without waiting a full tick.
func (w *MonitorWatchdog) Run(ctx context.Context) {
	w.check()

	ticker := time.NewTicker(w.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.check()
		}
	}
}

// Status returns the most recent snapshot. Safe for concurrent readers.
func (w *MonitorWatchdog) Status() MonitorStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

func (w *MonitorWatchdog) check() {
	now := w.now()
	next := MonitorStatus{HeartbeatFile: w.path}

	info, err := os.Stat(w.path)
	switch {
	case err == nil:
		next.Present = true
		next.LastHeartbeat = info.ModTime().UTC()
		age := max(now.Sub(info.ModTime()), 0)
		next.AgeSeconds = int64(age.Seconds())
		next.Stale = age > w.staleAfter
	case errors.Is(err, fs.ErrNotExist):
		next.Stale = true
	default:
		w.logger.Warn("monitor.watchdog.stat", "err", err)
		next.Stale = true
	}

	w.mu.Lock()
	prev := w.status
	w.status = next
	// Recover while the loop is stale, rate-limited by recoveryCD. A simple
	// cooldown (instead of transition-based firing) guarantees that a fresh
	// stale run after the cooldown window always retries, even if an
	// intermediate alive flap masked the prior transition.
	shouldRecover := next.Stale
	if shouldRecover && !w.lastRecoveryAt.IsZero() && now.Sub(w.lastRecoveryAt) < w.recoveryCD {
		shouldRecover = false
	}
	if shouldRecover {
		w.lastRecoveryAt = now
	}
	w.mu.Unlock()

	w.emit(events.MonitorHeartbeat, next)

	switch {
	case !prev.Stale && next.Stale:
		metrics.MonitorStaleTransition("stale")
		w.logger.Warn("monitor.watchdog.stale",
			"path", w.path,
			"age_s", next.AgeSeconds,
			"present", next.Present)
	case prev.Stale && !next.Stale:
		metrics.MonitorStaleTransition("alive")
		w.logger.Info("monitor.watchdog.alive",
			"path", w.path,
			"age_s", next.AgeSeconds)
	}

	if shouldRecover && w.recoverFn != nil {
		metrics.MonitorRecovery()
		w.logger.Info("monitor.watchdog.recover")
		w.recoverFn()
	}
}
