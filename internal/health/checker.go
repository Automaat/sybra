package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"path/filepath"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/procstat"
	"github.com/Automaat/sybra/internal/runacct"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/task"
)

const (
	TickInterval = 10 * time.Minute
	lookback     = 24 * time.Hour
	weekLookback = 7 * 24 * time.Hour
)

// Checker runs periodic health checks on audit data and task state.
type Checker struct {
	auditDir string
	tasks    *task.Manager
	homeDir  string
	logger   *slog.Logger
	emit     func(string, any)
	owned    func() OwnedProcesses
	docker   dockerRunner

	// sandboxQuarantine returns the currently quarantined sandbox cleanup
	// failures (see sandbox.Manager.QuarantinedEntries). nil disables the
	// check — set only when a sandbox.Manager is wired in (see New).
	sandboxQuarantine func() []sandbox.QuarantineEntry
	pressure          func() *PressureStatus
	// ghAuthProbe, when set, is called once per tick with a live
	// github.Authenticated() result plus the shared auth-health state
	// (github.AuthHealthSnapshot().State, passed as a plain string so this
	// package doesn't need to import internal/github) so credential loss is
	// caught proactively (checkGHAuthUnavailable) instead of only after a
	// push/issue-filing attempt has already failed, and a permanent
	// misconfiguration can be told apart from a transient blip. nil disables
	// the check — set only when GitHub integration is enabled (see
	// SetGHAuthProbe).
	ghAuthProbe func() (authenticated bool, state string)

	mu     sync.RWMutex
	report *Report
}

// New creates a Checker. Call Run to start the ticker loop.
func New(
	auditDir string,
	tasks *task.Manager,
	homeDir string,
	logger *slog.Logger,
	emit func(string, any),
	owned func() OwnedProcesses,
) *Checker {
	return &Checker{
		auditDir: auditDir,
		tasks:    tasks,
		homeDir:  homeDir,
		logger:   logger,
		emit:     emit,
		owned:    owned,
	}
}

// SetSandboxQuarantine wires in the sandbox quarantine source (see
// sandbox.Manager.QuarantinedEntries), enabling checkSandboxCleanupFailures.
// Optional — omit to skip the check entirely (e.g. in tests with no sandbox
// manager).
func (c *Checker) SetSandboxQuarantine(f func() []sandbox.QuarantineEntry) {
	c.sandboxQuarantine = f
}

// SetPressureStatus wires in the current pressure/reclaim telemetry source.
// Optional — omit to leave pressure telemetry out of the report.
func (c *Checker) SetPressureStatus(f func() *PressureStatus) {
	c.pressure = f
}

// SetGHAuthProbe wires in a live GitHub-auth probe (typically
// github.Authenticated paired with github.AuthHealthSnapshot().State),
// enabling checkGHAuthUnavailable. Optional — omit when GitHub integration is
// disabled, so an install with no gh CLI/token configured at all doesn't get
// a spurious critical finding every tick.
func (c *Checker) SetGHAuthProbe(f func() (authenticated bool, state string)) {
	c.ghAuthProbe = f
}

// OwnedProcesses separates exact Sybra PIDs from process groups Sybra created.
type OwnedProcesses struct {
	PIDs          map[int]bool
	ProcessGroups map[int]bool
}

// Owns reports whether pid is exact-owned or pgid is a trusted owned group.
func (o OwnedProcesses) Owns(pid, pgid int) bool {
	if pid > 0 && o.PIDs[pid] {
		return true
	}
	return pgid > 0 && o.ProcessGroups[pgid]
}

// Run blocks until ctx is done, running checks every TickInterval.
// Runs one check immediately on start.
func (c *Checker) Run(ctx context.Context) {
	c.check(ctx)

	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.check(ctx)
		}
	}
}

// LatestReport returns the most recent health report (nil if none yet).
func (c *Checker) LatestReport() *Report {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.report
}

func (c *Checker) check(ctx context.Context) {
	now := time.Now().UTC()
	since := now.Add(-lookback)

	dayEvents, err := audit.Read(c.auditDir, audit.Query{Since: since, Until: now})
	if err != nil {
		c.logger.Warn("health.check.audit_read", "err", err)
		dayEvents = nil
	}

	weekSince := now.Add(-weekLookback)
	weekEvents, err := audit.Read(c.auditDir, audit.Query{Since: weekSince, Until: now})
	if err != nil {
		c.logger.Warn("health.check.audit_read_week", "err", err)
		weekEvents = nil
	}

	tasks, err := c.tasks.List()
	if err != nil {
		c.logger.Warn("health.check.task_list", "err", err)
		tasks = nil
	}

	var findings []Finding
	findings = append(findings, checkFailureRate(dayEvents, now)...)
	findings = append(findings, checkCostOutliers(dayEvents, now)...)
	findings = append(findings, checkStuckTasks(dayEvents, tasks, now)...)
	findings = append(findings, checkWorkflowLoops(dayEvents, now)...)
	findings = append(findings, checkStatusBounce(dayEvents, now)...)
	findings = append(findings, checkCostDrift(dayEvents, weekEvents, now)...)
	findings = append(findings, checkAgentRetryLoops(dayEvents, now)...)
	findings = append(findings, checkTriageMismatch(weekEvents, now)...)
	findings = append(findings, checkStatusBottleneck(weekEvents, now)...)
	findings = append(findings, checkGHIssueAuthFailure(dayEvents, now)...)
	findings = append(findings, checkGHPushAuthFailure(dayEvents, now)...)
	if c.ghAuthProbe != nil {
		authenticated, state := c.ghAuthProbe()
		findings = append(findings, checkGHAuthUnavailable(authenticated, state, now)...)
	}
	docker := sampleDockerDisk(ctx, c.docker, now)
	findings = append(findings, checkDockerReclaimable(docker, now)...)
	if c.sandboxQuarantine != nil {
		findings = append(findings, checkSandboxCleanupFailures(c.sandboxQuarantine(), now)...)
	}

	for i := range findings {
		findings[i].Fingerprint = FingerprintFor(&findings[i])
	}

	stats := buildStats(dayEvents)
	owned := OwnedProcesses{}
	if c.owned != nil {
		owned = c.owned()
	}
	processes := procstat.Sample(5, owned.Owns)
	var pressure *PressureStatus
	if c.pressure != nil {
		pressure = sanitizePressureStatus(c.pressure())
	}

	report := &Report{
		GeneratedAt: now,
		PeriodStart: since,
		PeriodEnd:   now,
		Score:       RollupScore(findings),
		Findings:    findings,
		Stats:       stats,
		Pressure:    pressure,
		Processes:   &processes,
	}
	if docker.Available {
		report.Docker = &docker
	}

	c.mu.Lock()
	c.report = report
	c.mu.Unlock()

	c.persist(report)

	if c.emit != nil {
		c.emit("health:report", map[string]any{
			"findings": len(findings),
			"stats":    stats,
		})
	}

	c.logger.Info("health.check.done", "findings", len(findings),
		"total_cost", stats.TotalCostUSD, "failure_rate", stats.FailureRate)
}

// sanitizePressureStatus clamps NaN/Inf sample readings to -1 ("signal
// unreadable" — DiskFreePct/MemAvailablePct/LoadPerCPU are never legitimately
// negative). pressure.Gate.Status legitimately returns NaN for a signal it
// could not sample (see internal/pressure's allSignalsUnreadable and the CLI's
// own math.IsNaN-aware formatHealthPercent/formatHealthNumber), but
// encoding/json refuses to marshal NaN/Inf anywhere in the object graph —
// unsanitized, one unreadable sample would silently fail persist's
// MarshalIndent call and blackhole the *entire* report (score, findings,
// stats, docker, processes — not just pressure).
func sanitizePressureStatus(p *PressureStatus) *PressureStatus {
	if p == nil {
		return nil
	}
	sanitized := *p
	sanitized.DiskFreePct = safeFloat(p.DiskFreePct)
	sanitized.MemAvailablePct = safeFloat(p.MemAvailablePct)
	sanitized.LoadPerCPU = safeFloat(p.LoadPerCPU)
	return &sanitized
}

func safeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return -1
	}
	return v
}

// BuildStats computes run/failure/cost stats from an audit event stream —
// the same accounting internal/evaluation.Compute uses for its own run
// totals (runacct.Count with CountsTowardCodeAuthorFailureRate), exported so
// evaluation.ReconcileReports can prove the two agree over the same event
// window instead of re-deriving an equivalent computation that could drift
// from this one silently.
func BuildStats(events []audit.Event) Stats {
	return buildStats(events)
}

func buildStats(events []audit.Event) Stats {
	s := Stats{CostByRole: make(map[string]float64)}
	records := audit.RunRecords(events)
	counts := runacct.Count(records, nil, runacct.CountConfig{
		CountsTowardFailure: runacct.CountsTowardCodeAuthorFailureRate,
	})
	s.TotalAgentRuns = counts.Runs
	s.ResolvedRuns = counts.Resolved
	s.StalledRuns = counts.Stalled
	s.UnknownRuns = counts.Unknown
	s.FailedAgentRuns = counts.Failures
	for i := range records {
		s.TotalCostUSD += records[i].CostUSD
		s.CostByRole[roleLabel(records[i].Role)] += records[i].CostUSD
	}

	if s.ResolvedRuns > 0 {
		s.FailureRate = round2(float64(s.FailedAgentRuns) / float64(s.ResolvedRuns))
	}
	s.TotalCostUSD = round2(s.TotalCostUSD)
	for k, v := range s.CostByRole {
		s.CostByRole[k] = round2(v)
	}

	return s
}

// persist atomically writes r to disk. Using a temp-file+rename (via
// fsutil.AtomicWrite) instead of a plain os.WriteFile means a crash or
// disk-full mid-write can never leave behind a truncated report — readers
// (selfmonitor, `sybra-cli` inspection commands) always see either the
// previous good report or the new one, never a half-written file.
func (c *Checker) persist(r *Report) {
	path := filepath.Join(c.homeDir, "health-report.json")
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		c.logger.Warn("health.persist.marshal", "err", err)
		return
	}
	if err := fsutil.AtomicWrite(path, data); err != nil {
		c.logger.Warn("health.persist.write", "err", err)
	}
}
