package sybra

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/harnessevolution"
	"github.com/Automaat/sybra/internal/selfmonitor"
	"github.com/Automaat/sybra/internal/task"
)

// SelfMonitorService exposes the instance's own self-monitor corpus.
//
// The report, the ledger, and the health file all live under the server's
// home, and harness evolution mines the same report. A client reading its own
// copies would report one machine's findings and, with --file, create the
// tasks on another machine's board.
type SelfMonitorService struct {
	tasks         *task.Manager
	logger        *slog.Logger
	currentConfig func() *config.Config
}

func (s *SelfMonitorService) config() *config.Config {
	if s.currentConfig == nil {
		return nil
	}
	return s.currentConfig()
}

// GetSelfMonitorReport returns the report the background loop persisted on its last tick.
func (s *SelfMonitorService) GetSelfMonitorReport() (selfmonitor.Report, error) {
	data, err := os.ReadFile(config.SelfMonitorLastReportPath())
	if err != nil {
		if os.IsNotExist(err) {
			return selfmonitor.Report{}, unavailableError(
				"no selfmonitor report yet (run an investigate pass, or start sybra with self_monitor.enabled=true)")
		}
		return selfmonitor.Report{}, err
	}
	var report selfmonitor.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return selfmonitor.Report{}, err
	}
	return report, nil
}

// InvestigateSelfMonitor runs one preview pass.
//
// It builds its own service rather than reusing the background one on purpose:
// this pass persists nothing, files no issues, and runs with no judge or
// actor, so an operator previewing a tick cannot trigger remediation. Enabled
// is forced so the preview works before the config block is switched on.
func (s *SelfMonitorService) InvestigateSelfMonitor() (selfmonitor.Report, error) {
	cfg := s.config()
	if cfg == nil || s.tasks == nil {
		return selfmonitor.Report{}, unavailableError("self-monitor unavailable")
	}
	ledger, err := selfmonitor.Open(config.SelfMonitorLedgerPath())
	if err != nil {
		return selfmonitor.Report{}, err
	}
	scfg := cfg.SelfMonitor
	scfg.Enabled = true
	svc := selfmonitor.NewService(selfmonitor.Deps{
		Cfg:     scfg,
		Tasks:   s.tasks,
		Health:  selfmonitor.DiskHealthReader{Path: config.HealthReportPath()},
		Ledger:  ledger,
		LogsDir: cfg.Logging.Dir,
	})
	return svc.Scan(context.Background())
}

// ListSelfMonitorLedger returns ledger entries oldest first, optionally
// filtered to one fingerprint and to a window measured back from now.
func (s *SelfMonitorService) ListSelfMonitorLedger(fingerprint string, windowSeconds int64) ([]selfmonitor.LedgerEntry, error) {
	ledger, err := selfmonitor.Open(config.SelfMonitorLedgerPath())
	if err != nil {
		return nil, err
	}
	window := time.Duration(windowSeconds) * time.Second
	var entries []selfmonitor.LedgerEntry
	if fingerprint != "" {
		entries = ledger.History(fingerprint, window)
	} else {
		entries = ledger.Entries(window)
	}
	slices.SortFunc(entries, func(a, b selfmonitor.LedgerEntry) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	if entries == nil {
		entries = []selfmonitor.LedgerEntry{}
	}
	return entries, nil
}

// HarnessEvolutionRunDTO pairs a mining run with the tasks it filed.
type HarnessEvolutionRunDTO struct {
	Result harnessevolution.RunResult `json:"result"`
	Filed  []task.Task                `json:"filed"`
}

// RunHarnessEvolution mines the instance's persisted self-monitor report for
// proposals, and files the accepted ones when asked.
//
// Mining and filing are one call because they read and write the same
// instance: splitting them would let a client mine one machine's report and
// file the proposals on another machine's board.
func (s *SelfMonitorService) RunHarnessEvolution(lookbackSeconds int64, minClusterSize int, corpusDir string, fileTasks bool) (HarnessEvolutionRunDTO, error) {
	cfg := s.config()
	if cfg == nil || s.tasks == nil {
		return HarnessEvolutionRunDTO{}, unavailableError("harness evolution unavailable")
	}
	lookback := time.Duration(lookbackSeconds) * time.Second
	if lookback <= 0 {
		lookback = time.Duration(cfg.HarnessEvolve.LookbackHours * float64(time.Hour))
	}
	if minClusterSize <= 0 {
		minClusterSize = cfg.HarnessEvolve.MinClusterSize
	}
	result, err := harnessevolution.Run(context.Background(), harnessevolution.Options{
		ReportPath:          config.SelfMonitorLastReportPath(),
		CorpusDir:           corpusDir,
		OutputDir:           config.HarnessEvolveDir(),
		Lookback:            lookback,
		MinClusterSize:      minClusterSize,
		MaxReportAge:        time.Duration(cfg.HarnessEvolve.MaxReportAgeHours * float64(time.Hour)),
		SelfMonitorEnabled:  cfg.SelfMonitor.Enabled,
		SelfMonitorInterval: time.Duration(cfg.SelfMonitor.IntervalHours * float64(time.Hour)),
	})
	if err != nil {
		return HarnessEvolutionRunDTO{}, err
	}
	out := HarnessEvolutionRunDTO{Result: result, Filed: []task.Task{}}
	if !fileTasks {
		return out, nil
	}
	filed, err := harnessevolution.FileProposals(s.tasks, result)
	if err != nil {
		return out, err
	}
	if filed != nil {
		out.Filed = filed
	}
	return out, nil
}
