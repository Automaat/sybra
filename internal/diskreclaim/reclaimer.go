// Package diskreclaim triggers an automatic, rate-limited, safe-cache
// cleanup pass when internal/pressure.Gate observes disk space crossing its
// warning watermark — reclaiming build and disposable runtime caches before
// dispatch is blocked by the gate's critical (MinDiskFreePercent) threshold.
//
// Scope is deliberately narrow: only internal/cleanup's RiskSafe buckets
// (logs, audit, orphaned sandboxes, orphaned per-task Go build caches) are
// ever deleted automatically here. Destructive buckets (git worktrees,
// shared go-mod/npm caches) and the report-only external (docker) bucket are
// never touched — reclaiming those requires an explicit `sybra-cli doctor
// cleanup --worktrees/--external --apply`. Their size is still sampled
// (never deleted) and reported as "unreclaimable" telemetry so an operator
// can see how much space exists outside the automatic safe path. Active
// task/agent resources are protected the same way internal/cleanup already
// protects them for the manual CLI path: a resource is only ever eligible
// once its owning task is orphaned or terminal/blocked and past retention
// (see internal/cleanup's eligible/revalidate).
package diskreclaim

import (
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/config"
)

// DefaultCooldown rate-limits automatic reclaim passes when the caller
// supplies a non-positive cooldown to New.
const DefaultCooldown = 5 * time.Minute

// SafeBuckets are the internal/cleanup RiskSafe bucket names this package
// applies automatically. See the package doc for why destructive buckets are
// excluded from automatic deletion.
var SafeBuckets = []string{cleanup.BucketLogs, cleanup.BucketAudit, cleanup.BucketSandboxes, cleanup.BucketGoBuildCache}

// unreclaimableBuckets are scanned (report-only, never applied) so their
// size can be surfaced as "space that exists but automatic cleanup won't
// touch."
var unreclaimableBuckets = []string{cleanup.BucketWorktrees, cleanup.BucketSharedCache}

// Outcome summarizes one automatic safe-cache reclaim pass, for health
// telemetry.
type Outcome struct {
	RanAt              time.Time
	ReclaimedBytes     int64
	UnreclaimableBytes int64
	Errors             int
}

// Reclaimer runs bounded, idempotent, rate-limited safe-cache cleanup passes
// over one Sybra home directory. Safe for concurrent use; TryRun never
// blocks the caller and coalesces concurrent triggers into at most one
// in-flight pass.
type Reclaimer struct {
	scanner  *cleanup.Scanner
	cooldown time.Duration
	logger   *slog.Logger

	mu      sync.Mutex
	running bool
	lastRun time.Time
	last    Outcome
}

// New builds a Reclaimer over cfg's resolved directories and tasks. cooldown
// <= 0 falls back to DefaultCooldown.
func New(cfg *config.Config, tasks cleanup.TaskLister, cooldown time.Duration, logger *slog.Logger) *Reclaimer {
	if cooldown <= 0 {
		cooldown = DefaultCooldown
	}
	return &Reclaimer{
		scanner:  cleanup.NewScanner(cfg, tasks),
		cooldown: cooldown,
		logger:   logger,
	}
}

// TryRun starts a reclaim pass in the background if the cooldown has
// elapsed since the last pass and no pass is already running, and reports
// whether it did. A nil Reclaimer always returns false. Safe to call from a
// hot path (e.g. pressure.Gate.Admit): the in-flight/cooldown check is
// synchronous and cheap, and the actual filesystem work always happens in a
// background goroutine.
func (r *Reclaimer) TryRun() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.running || (!r.lastRun.IsZero() && time.Since(r.lastRun) < r.cooldown) {
		r.mu.Unlock()
		return false
	}
	r.running = true
	r.mu.Unlock()

	go r.run()
	return true
}

// run performs one reclaim pass: apply the safe buckets, then scan (never
// apply) the destructive buckets to size the unreclaimable remainder.
func (r *Reclaimer) run() {
	defer func() {
		r.mu.Lock()
		r.running = false
		r.lastRun = time.Now()
		r.mu.Unlock()
	}()

	outcome := Outcome{RanAt: time.Now()}

	safeOpts := cleanup.Options{Only: SafeBuckets}
	scanResult, err := r.scanner.Scan(safeOpts)
	if err != nil {
		r.logWarn("diskreclaim.scan", err)
	} else {
		applyResult, err := r.scanner.Apply(scanResult.Buckets, safeOpts)
		if err != nil {
			r.logWarn("diskreclaim.apply", err)
		} else {
			for _, br := range applyResult.Buckets {
				outcome.ReclaimedBytes += br.ReclaimedBytes
				outcome.Errors += len(br.Errors)
			}
		}
	}

	unreclaimOpts := cleanup.Options{Only: unreclaimableBuckets, Worktrees: true, External: true}
	if unreclaimScan, err := r.scanner.Scan(unreclaimOpts); err != nil {
		r.logWarn("diskreclaim.scan_unreclaimable", err)
	} else {
		for _, b := range unreclaimScan.Buckets {
			outcome.UnreclaimableBytes += b.Bytes
		}
	}

	if r.logger != nil {
		r.logger.Info("diskreclaim.done",
			"reclaimed_bytes", outcome.ReclaimedBytes,
			"unreclaimable_bytes", outcome.UnreclaimableBytes,
			"errors", outcome.Errors)
	}

	r.mu.Lock()
	r.last = outcome
	r.mu.Unlock()
}

func (r *Reclaimer) logWarn(msg string, err error) {
	if r.logger != nil {
		r.logger.Warn(msg, "err", err)
	}
}

// LastOutcome returns the most recent reclaim pass outcome (the zero value
// if none has run yet). Safe on a nil Reclaimer.
func (r *Reclaimer) LastOutcome() Outcome {
	if r == nil {
		return Outcome{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}
