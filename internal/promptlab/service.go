package promptlab

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Automaat/sybra/internal/stats"
)

// Options configures one Run.
type Options struct {
	ReportPath    string
	Records       []stats.RunRecord
	OutputDir     string
	Lookback      time.Duration
	MinSamples    int
	MinEffectSize float64
	MaxProposals  int
	Evaluator     OfflineEvaluator
	Now           time.Time
	Logger        *slog.Logger
}

// Run orchestrates collect -> propose -> evaluate -> SaveRunResult. A
// missing evaluation report is treated as "no evidence yet" (empty
// RunResult, nil error); a present-but-malformed report is a genuine error
// and Run returns early without writing anything, so a parse failure can
// never be mistaken for "no weak subjects" and silently persisted.
func Run(ctx context.Context, opts Options) (RunResult, error) {
	select {
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	default:
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	report, err := loadEvaluationReport(opts.ReportPath)
	if err != nil {
		return RunResult{}, err
	}

	records := withinLookback(opts.Records, opts.Lookback, now)
	subjects := CollectWeakSubjects(report, records, opts.MinSamples, opts.MinEffectSize)
	proposals, dropped := Propose(subjects, opts.MaxProposals, now)
	if dropped > 0 {
		logger.Info("promptlab.capped", "dropped", dropped, "kept", len(proposals))
	}

	evaluator := opts.Evaluator
	if evaluator == nil {
		evaluator = stubEvaluator{}
	}
	for i := range proposals {
		proposals[i] = EvaluateProposal(proposals[i], evaluator)
	}

	result := RunResult{
		GeneratedAt:  now,
		WeakSubjects: subjects,
		Proposals:    proposals,
		Dropped:      dropped,
	}
	if opts.OutputDir != "" {
		if err := SaveRunResult(opts.OutputDir, result); err != nil {
			return RunResult{}, err
		}
	}
	return result, nil
}

// SaveRunResult persists the most recent run snapshot to <dir>/last-run.json.
func SaveRunResult(dir string, result RunResult) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "last-run.json"), data, 0o600)
}
