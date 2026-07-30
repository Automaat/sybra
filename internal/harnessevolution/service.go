package harnessevolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/selfmonitor"
)

// DefaultMaxReportAge bounds how old a self-monitor report can be before Run
// treats it as stale rather than clustering proposals from possibly-ancient
// data. A report older than this most likely means self-monitor stopped
// ticking (disabled mid-flight, crashed, config error) rather than that
// nothing happened recently.
const DefaultMaxReportAge = 48 * time.Hour

type Options struct {
	ReportPath     string
	CorpusDir      string
	OutputDir      string
	Lookback       time.Duration
	MinClusterSize int
	Now            time.Time

	// SelfMonitorEnabled mirrors config.SelfMonitorConfig.Enabled. It only
	// refines the degraded outcome when no usable report exists: with
	// self-monitor disabled no fresher report will ever be produced, so a
	// missing report is reported as StateDisabled rather than "not ticked
	// yet". A fresh report on disk is always consumed regardless of this
	// flag — disabling self-monitor to stop background spend must not throw
	// away telemetry it already wrote. Staleness (MaxReportAge) is what
	// guards against reading data that has gone stale after it stopped
	// ticking.
	SelfMonitorEnabled bool
	// MaxReportAge is the freshness window used to detect a stale report.
	// 0 uses DefaultMaxReportAge.
	MaxReportAge time.Duration
}

// Run loads the self-monitor report, clusters its actionable findings, and
// proposes harness changes. When the report is missing, stale, or unreadable,
// Run returns a RunResult with State set to the matching
// selfmonitor.PipelineState and skips clustering entirely rather than
// proposing anything from data it doesn't trust. A fresh report is consumed
// even when self-monitor is currently disabled — the disabled flag only
// refines the reason reported for a missing report.
func Run(ctx context.Context, opts Options) (RunResult, error) {
	select {
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	default:
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	report, err := LoadSelfMonitorReport(opts.ReportPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No report on disk. If self-monitor is disabled it will never
			// produce one; otherwise it simply has not ticked yet.
			if !opts.SelfMonitorEnabled {
				return finishDegraded(opts, RunResult{
					GeneratedAt: now,
					State:       selfmonitor.StateDisabled,
					Reason:      "self_monitor.enabled=false and no report on disk: self-monitor never produces the report harness-evolution reads",
				})
			}
			return finishDegraded(opts, RunResult{
				GeneratedAt: now,
				State:       selfmonitor.StateStale,
				Reason:      fmt.Sprintf("no self-monitor report found at %s: self-monitor has not ticked yet", opts.ReportPath),
			})
		}
		return finishDegraded(opts, RunResult{
			GeneratedAt: now,
			State:       selfmonitor.StateFailed,
			Reason:      fmt.Sprintf("load self-monitor report: %v", err),
		})
	}

	maxAge := opts.MaxReportAge
	if maxAge <= 0 {
		maxAge = DefaultMaxReportAge
	}
	if age := now.Sub(report.GeneratedAt); age > maxAge {
		reason := fmt.Sprintf("self-monitor report is %s old (max %s): self-monitor may be disabled or stuck",
			age.Round(time.Second), maxAge)
		if !opts.SelfMonitorEnabled {
			reason = fmt.Sprintf("self-monitor report is %s old (max %s) and self_monitor.enabled=false: no fresher report will be produced",
				age.Round(time.Second), maxAge)
		}
		return finishDegraded(opts, RunResult{
			GeneratedAt: now,
			State:       selfmonitor.StateStale,
			Reason:      reason,
		})
	}

	since := time.Time{}
	if opts.Lookback > 0 {
		since = now.Add(-opts.Lookback)
	}
	events := EventsFromReport(report, since)
	clusters := ClusterEvents(events, opts.MinClusterSize)
	proposals := Propose(clusters, now)
	corpus, err := LoadCorpusDir(opts.CorpusDir)
	if err != nil {
		return RunResult{}, fmt.Errorf("load corpus: %w", err)
	}
	for i := range proposals {
		proposals[i].Evaluation = EvaluateProposal(proposals[i], clusters[i], corpus)
	}
	result := RunResult{
		GeneratedAt: now,
		Events:      len(events),
		Clusters:    clusters,
		Proposals:   proposals,
		State:       selfmonitor.StateHealthy,
	}
	if report.State == selfmonitor.StatePartial {
		result.State = selfmonitor.StatePartial
		result.Reason = fmt.Sprintf(
			"self-monitor report covered %d/%d findings (%d truncated records, %d analysis errors)",
			report.InputsAnalyzed, report.InputsTotal, report.TruncatedRecords, report.AnalysisErrors)
	}
	if opts.OutputDir != "" {
		if err := SaveRunResult(opts.OutputDir, result); err != nil {
			return RunResult{}, err
		}
	}
	return result, nil
}

// finishDegraded persists (when configured) and returns a degraded
// RunResult without attempting to cluster or propose against data that
// either doesn't exist or can't be trusted.
func finishDegraded(opts Options, result RunResult) (RunResult, error) {
	if opts.OutputDir != "" {
		if err := SaveRunResult(opts.OutputDir, result); err != nil {
			return RunResult{}, err
		}
	}
	return result, nil
}

// SaveRunResult atomically persists result to <dir>/last-run.json via
// fsutil.AtomicWrite (temp file + rename), so a crash or disk-full mid-write
// can never leave behind a truncated file — readers always see either the
// previous run's result or the new one.
func SaveRunResult(dir string, result RunResult) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "last-run.json")
	return fsutil.AtomicWrite(path, data)
}
