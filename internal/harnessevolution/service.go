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

type Options struct {
	ReportPath     string
	CorpusDir      string
	OutputDir      string
	Lookback       time.Duration
	MinClusterSize int
	// MaxReportAge bounds how stale the persisted self-monitor report may be
	// before its findings are discarded. Zero disables the guard. Without it a
	// "last good" report keeps driving proposals via Lookback long after
	// self-monitor stops ticking (see config.HarnessEvolveConfig).
	MaxReportAge time.Duration
	Now          time.Time
}

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
	missingReport := false
	report, err := LoadSelfMonitorReport(opts.ReportPath)
	if err != nil {
		// A fresh/default home has no self-monitor report yet. Treat a missing
		// report as an empty one so the command degrades to a harmless
		// zero-proposal run instead of hard-failing before the first report
		// exists. Any other read/parse error is still fatal. Flag it so the
		// zero-proposal run reads as "pipeline disconnected", not "clean data" —
		// otherwise a fresh GeneratedAt.IsZero() report also dodges the
		// stale-report guard and the automation files nothing with no signal.
		if !errors.Is(err, os.ErrNotExist) {
			return RunResult{}, err
		}
		report = selfmonitor.Report{}
		missingReport = true
	}
	staleReport := false
	if opts.MaxReportAge > 0 && !report.GeneratedAt.IsZero() &&
		now.Sub(report.GeneratedAt) > opts.MaxReportAge {
		// The report predates MaxReportAge: self-monitor has stopped writing
		// fresh evidence. Discard its findings so a stale "last good" report
		// can't keep minting proposals until its findings age out of Lookback.
		staleReport = true
		report = selfmonitor.Report{}
	}
	degradedReport := false
	if report.Degraded {
		// A degraded tick produced only partial evidence (a finding's agent log
		// was unreadable or truncated, or the tick failed before finishing).
		// Discard its findings so incomplete coverage can't mint proposals before
		// the missing log coverage is repaired.
		degradedReport = true
		report = selfmonitor.Report{}
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
		GeneratedAt:    now,
		Events:         len(events),
		Clusters:       clusters,
		Proposals:      proposals,
		StaleReport:    staleReport,
		MissingReport:  missingReport,
		DegradedReport: degradedReport,
	}
	if opts.OutputDir != "" {
		if err := SaveRunResult(opts.OutputDir, result); err != nil {
			return RunResult{}, err
		}
	}
	return result, nil
}

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
