package selfmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/task"
)

// stubHealth returns a fixed health report — tests set .Report directly.
type stubHealth struct {
	Report *health.Report
	Err    error
}

func (s *stubHealth) LatestReport() (*health.Report, error) {
	return s.Report, s.Err
}

// stubTasks satisfies TaskAPI with an in-memory map, no filesystem.
type stubTasks struct {
	byID map[string]task.Task
}

func (s *stubTasks) Get(id string) (task.Task, error) {
	t, ok := s.byID[id]
	if !ok {
		return task.Task{}, os.ErrNotExist
	}
	return t, nil
}

func (s *stubTasks) List() ([]task.Task, error) {
	out := make([]task.Task, 0, len(s.byID))
	for id := range s.byID {
		out = append(out, s.byID[id])
	}
	return out, nil
}

func newServiceForTest(t *testing.T, h HealthReader, tasks TaskAPI, logsDir string) *Service {
	t.Helper()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, err := Open(ledgerPath)
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}
	return NewService(Deps{
		Cfg: config.SelfMonitorConfig{
			Enabled:              true,
			IntervalHours:        6,
			SuppressionDays:      7,
			SuppressionThreshold: 3,
		},
		Tasks:          tasks,
		Health:         h,
		Ledger:         l,
		LogsDir:        logsDir,
		LastReportPath: filepath.Join(dir, "last-report.json"),
	})
}

func TestScanNoHealthReport(t *testing.T) {
	svc := newServiceForTest(t, &stubHealth{Report: nil}, nil, "")
	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 0 {
		t.Errorf("Findings = %d, want 0", len(r.Findings))
	}
	if r.SchemaVersion != ReportSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", r.SchemaVersion, ReportSchemaVersion)
	}
}

func TestScanDistillsLogs(t *testing.T) {
	// Stage an agent log file and point the finding at it. The analyzer
	// fixture from loganalyzer_test.go already exercises the parser — this
	// test just verifies the service threads the path through.
	logsDir := t.TempDir()
	agentDir := filepath.Join(logsDir, "agents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(agentDir, "agent-abc-2026-04-14T10-00-00.ndjson")
	if err := os.WriteFile(logPath, []byte(strings.Join(fixtureLines(), "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	rep := &health.Report{
		GeneratedAt: time.Now().UTC(),
		Score:       health.ScoreWarning,
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Severity:    health.SeverityWarning,
			Title:       "eval agent cost $0.65",
			Fingerprint: "cost_outlier:task-1",
			TaskID:      "task-1",
			AgentID:     "agent-abc",
			LogFile:     logPath,
		}},
	}
	svc := newServiceForTest(t, &stubHealth{Report: rep}, nil, logsDir)

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(r.Findings))
	}
	inv := r.Findings[0]
	if inv.LogSummary == nil {
		t.Fatal("LogSummary = nil, want analyzed summary")
	}
	if inv.LogSummary.TotalToolCalls != 5 {
		t.Errorf("TotalToolCalls = %d, want 5", inv.LogSummary.TotalToolCalls)
	}
	if inv.Verdict.Classification != VerdictPending {
		t.Errorf("Verdict = %q, want %q", inv.Verdict.Classification, VerdictPending)
	}
}

func TestScanResolvesLogFromTaskAgentRuns(t *testing.T) {
	// LogFile missing from the finding but present on the task's AgentRun.
	logsDir := t.TempDir()
	agentDir := filepath.Join(logsDir, "agents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(agentDir, "agent-xyz.ndjson")
	if err := os.WriteFile(logPath, []byte(strings.Join(fixtureLines(), "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	tasks := &stubTasks{byID: map[string]task.Task{
		"task-2": {
			ID: "task-2",
			AgentRuns: []task.AgentRun{
				{AgentID: "agent-xyz", LogFile: logPath},
			},
		},
	}}
	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatAgentRetryLoop,
			Fingerprint: "agent_retry_loop:task-2",
			TaskID:      "task-2",
			AgentID:     "agent-xyz",
		}},
	}
	svc := newServiceForTest(t, &stubHealth{Report: rep}, tasks, logsDir)

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 1 || r.Findings[0].LogSummary == nil {
		t.Fatalf("expected 1 finding with log summary, got %+v", r.Findings)
	}
}

func TestScanAutoSuppressesChronicFalsePositive(t *testing.T) {
	logsDir := t.TempDir()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, err := Open(ledgerPath)
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
	}
	fp := "cost_outlier:task-noisy"
	for range 3 {
		if err := l.Append(LedgerEntry{
			Fingerprint: fp,
			Verdict:     VerdictFalsePositive,
			CreatedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}

	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Fingerprint: fp,
			TaskID:      "task-noisy",
		}},
	}
	svc := NewService(Deps{
		Cfg: config.SelfMonitorConfig{
			Enabled:              true,
			IntervalHours:        6,
			SuppressionDays:      7,
			SuppressionThreshold: 3,
		},
		Health:  &stubHealth{Report: rep},
		Ledger:  l,
		LogsDir: logsDir,
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 0 {
		t.Errorf("suppressed finding leaked: %+v", r.Findings)
	}
	if r.Suppressed != 1 {
		t.Errorf("Suppressed = %d, want 1", r.Suppressed)
	}
}

func TestScanFiltersByProjectType(t *testing.T) {
	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatStuckTask,
			Fingerprint: "stuck_task:task-a",
			TaskID:      "task-a",
		}},
	}
	tasks := &stubTasks{byID: map[string]task.Task{
		"task-a": {ID: "task-a", ProjectID: "owner/repo"},
	}}
	svc := NewService(Deps{
		Cfg:    config.SelfMonitorConfig{Enabled: true, IntervalHours: 6},
		Tasks:  tasks,
		Health: &stubHealth{Report: rep},
		// AllowsProject rejects owner/repo.
		AllowsProject: func(pid string) bool { return pid != "owner/repo" },
	})
	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 0 {
		t.Errorf("AllowsProject gate leaked: %+v", r.Findings)
	}
	if r.Suppressed != 1 {
		t.Errorf("Suppressed = %d, want 1", r.Suppressed)
	}
}

func TestPersistReportWritesFile(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "nested", "last-report.json")
	svc := NewService(Deps{
		Cfg:            config.SelfMonitorConfig{Enabled: true, IntervalHours: 6},
		Health:         &stubHealth{Report: &health.Report{Score: health.ScoreGood}},
		LastReportPath: reportPath,
	})
	svc.tickAndLog(context.Background())

	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("persisted file missing: %v", err)
	}
	var back Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("persisted file unparseable: %v", err)
	}
	if back.HealthScore != health.ScoreGood {
		t.Errorf("HealthScore = %q, want %q", back.HealthScore, health.ScoreGood)
	}
}

func TestDiskHealthReaderMissingFile(t *testing.T) {
	r := DiskHealthReader{Path: filepath.Join(t.TempDir(), "nope.json")}
	rep, err := r.LatestReport()
	if !errors.Is(err, ErrNoHealthReport) {
		t.Errorf("err = %v, want ErrNoHealthReport on missing file", err)
	}
	if rep != nil {
		t.Errorf("rep = %+v, want nil on missing file", rep)
	}
}

func TestDiskHealthReaderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	want := health.Report{
		GeneratedAt: time.Now().UTC().Round(time.Millisecond),
		Score:       health.ScoreCritical,
		Findings: []health.Finding{{
			Category:    health.CatFailureRate,
			Fingerprint: "failure_rate",
		}},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := DiskHealthReader{Path: path}
	got, err := r.LatestReport()
	if err != nil {
		t.Fatalf("LatestReport: %v", err)
	}
	if got == nil || got.Score != health.ScoreCritical {
		t.Errorf("got = %+v", got)
	}
}

func TestRunRespectsDisabled(t *testing.T) {
	svc := NewService(Deps{
		Cfg:    config.SelfMonitorConfig{Enabled: false},
		Health: &stubHealth{Report: &health.Report{}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
		// Good: Run returned immediately.
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return on cfg.Enabled=false")
	}
}
