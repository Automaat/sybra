package harnessevolution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Options struct {
	ReportPath     string
	CorpusDir      string
	OutputDir      string
	Lookback       time.Duration
	MinClusterSize int
	Now            time.Time
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
	report, err := LoadSelfMonitorReport(opts.ReportPath)
	if err != nil {
		return RunResult{}, err
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
	return os.WriteFile(path, data, 0o600)
}
