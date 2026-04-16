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
) *Checker {
	return &Checker{
		auditDir: auditDir,
		tasks:    tasks,
		homeDir:  homeDir,
		logger:   logger,
		emit:     emit,
	}
}

// Run blocks until ctx is done, running checks every TickInterval.
// Runs one check immediately on start.
func (c *Checker) Run(ctx context.Context) {
	c.check()

	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.check()
		}
	}
}

// LatestReport returns the most recent health report (nil if none yet).
func (c *Checker) LatestReport() *Report {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.report
}

func (c *Checker) check() {
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

	for i := range findings {
		findings[i].Fingerprint = FingerprintFor(&findings[i])
	}

	stats := buildStats(dayEvents)

	report := &Report{
		GeneratedAt: now,
		PeriodStart: since,
		PeriodEnd:   now,
		Score:       RollupScore(findings),
		Findings:    findings,
		Stats:       stats,
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

	for _, e := range events {
		switch e.Type {
		case audit.EventAgentCompleted:
			s.TotalAgentRuns++
			cost, _ := e.Data["cost_usd"].(float64)
			s.TotalCostUSD += cost
			role, _ := e.Data["role"].(string)
			s.CostByRole[roleLabel(role)] += cost
		case audit.EventAgentFailed:
			s.TotalAgentRuns++
			s.FailedAgentRuns++
		}
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
