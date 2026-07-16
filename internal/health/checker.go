package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/procstat"
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

	report := &Report{
		GeneratedAt: now,
		PeriodStart: since,
		PeriodEnd:   now,
		Score:       RollupScore(findings),
		Findings:    findings,
		Stats:       stats,
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

func buildStats(events []audit.Event) Stats {
	s := Stats{CostByRole: make(map[string]float64)}
	runs := audit.NormalizeAgentRuns(events)
	for i := range runs {
		run := &runs[i]
		if !run.Terminal {
			continue
		}
		s.TotalAgentRuns++
		if run.Failed {
			s.FailedAgentRuns++
		}
		cost, _ := run.TerminalEvent.Data["cost_usd"].(float64)
		s.TotalCostUSD += cost
		role, _ := run.TerminalEvent.Data["role"].(string)
		s.CostByRole[roleLabel(role)] += cost
	}

	if s.TotalAgentRuns > 0 {
		s.FailureRate = round2(float64(s.FailedAgentRuns) / float64(s.TotalAgentRuns))
	}
	s.TotalCostUSD = round2(s.TotalCostUSD)
	for k, v := range s.CostByRole {
		s.CostByRole[k] = round2(v)
	}

	return s
}

func (c *Checker) persist(r *Report) {
	path := filepath.Join(c.homeDir, "health-report.json")
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		c.logger.Warn("health.persist.marshal", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		c.logger.Warn("health.persist.write", "err", err)
	}
}
