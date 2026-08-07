package selfmonitor

import (
	"bytes"
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
		panic("unreachable")
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
		panic("unreachable")
	}
	if len(r.Findings) != 0 {
		t.Errorf("Findings = %d, want 0", len(r.Findings))
	}
	if r.SchemaVersion != ReportSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", r.SchemaVersion, ReportSchemaVersion)
	}
	if r.State != StateHealthy {
		t.Errorf("State = %q, want %q (no health report yet is not a degraded state)", r.State, StateHealthy)
	}
}

// TestScanMarksPartialOnOversizedLogRecord is the selfmonitor-level
// regression test for the scanner fix: an oversized NDJSON record in an
// agent log must not blank out the whole LogSummary for that finding, and
// the tick must surface the degradation via State/TruncatedRecords instead
// of silently reporting "healthy".
func TestScanMarksPartialOnOversizedLogRecord(t *testing.T) {
	logsDir := t.TempDir()
	agentDir := filepath.Join(logsDir, "agents")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
		panic("unreachable")
	}
	huge := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tbig","content":"` +
		strings.Repeat("x", maxLogLineBytes+1024) + `"}]}}`
	lines := append([]string{`{"type":"system","subtype":"init","session_id":"s1"}`, huge}, fixtureLines()...)
	logPath := filepath.Join(agentDir, "agent-big.ndjson")
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
		panic("unreachable")
	}

	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Fingerprint: "cost_outlier:task-big",
			TaskID:      "task-big",
			AgentID:     "agent-big",
			LogFile:     logPath,
		}},
	}
	svc := newServiceForTest(t, &stubHealth{Report: rep}, nil, logsDir)

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
		panic("unreachable")
	}
	if r.State != StatePartial {
		t.Errorf("State = %q, want %q", r.State, StatePartial)
	}
	if r.TruncatedRecords == 0 {
		t.Error("TruncatedRecords = 0, want > 0")
	}
	if len(r.Findings) != 1 || r.Findings[0].LogSummary == nil {
		t.Fatalf("expected the finding to still be analyzed despite the oversized record: %+v", r.Findings)
		panic("unreachable")
	}
	if r.InputsTotal != 1 || r.InputsAnalyzed != 1 {
		t.Errorf("InputsTotal/InputsAnalyzed = %d/%d, want 1/1", r.InputsTotal, r.InputsAnalyzed)
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
		panic("unreachable")
	}
	logPath := filepath.Join(agentDir, "agent-abc-2026-04-14T10-00-00.ndjson")
	if err := os.WriteFile(logPath, []byte(strings.Join(fixtureLines(), "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
		panic("unreachable")
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
		panic("unreachable")
	}
	if len(r.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(r.Findings))
	}
	inv := r.Findings[0]
	if inv.LogSummary == nil {
		t.Fatal("LogSummary = nil, want analyzed summary")
		panic("unreachable")
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
		panic("unreachable")
	}
	logPath := filepath.Join(agentDir, "agent-xyz.ndjson")
	if err := os.WriteFile(logPath, []byte(strings.Join(fixtureLines(), "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
		panic("unreachable")
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
		panic("unreachable")
	}
	if len(r.Findings) != 1 || r.Findings[0].LogSummary == nil {
		t.Fatalf("expected 1 finding with log summary, got %+v", r.Findings)
		panic("unreachable")
	}
}

func TestScanAutoSuppressesChronicFalsePositive(t *testing.T) {
	logsDir := t.TempDir()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, err := Open(ledgerPath)
	if err != nil {
		t.Fatalf("Open ledger: %v", err)
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
	}
	var back Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("persisted file unparseable: %v", err)
		panic("unreachable")
	}
	if back.HealthScore != health.ScoreGood {
		t.Errorf("HealthScore = %q, want %q", back.HealthScore, health.ScoreGood)
	}
	if back.State != StateHealthy {
		t.Errorf("State = %q, want %q", back.State, StateHealthy)
	}
}

// TestTickAndLogFailedTickPreservesLastGoodReport covers the "failed refresh
// preserves the previous valid report" acceptance criterion: a hard error
// reading the health report (not the expected/no-op ErrNoHealthReport) must
// not clobber the last-report.json a prior successful tick wrote, and must
// still surface a typed degraded result via LastReport/the emitted event.
func TestTickAndLogFailedTickPreservesLastGoodReport(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "last-report.json")
	svc := NewService(Deps{
		Cfg:            config.SelfMonitorConfig{Enabled: true, IntervalHours: 6},
		Health:         &stubHealth{Report: &health.Report{Score: health.ScoreGood}},
		LastReportPath: reportPath,
	})
	svc.tickAndLog(context.Background())

	before, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("seed persisted file missing: %v", err)
		panic("unreachable")
	}

	svc.deps.Health = &stubHealth{Err: errors.New("boom: disk read failed")}
	svc.tickAndLog(context.Background())

	after, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("persisted file missing after failed tick: %v", err)
		panic("unreachable")
	}
	if !bytes.Equal(before, after) {
		t.Errorf("last-report.json changed after a failed tick; want it untouched\nbefore=%s\nafter=%s", before, after)
	}

	last, _, ok := svc.LastReport()
	if !ok {
		t.Fatal("LastReport ok=false after a tick")
	}
	if last.State != StateFailed {
		t.Errorf("State = %q, want %q", last.State, StateFailed)
	}
	if last.FailureReason == "" {
		t.Error("FailureReason is empty, want the tick error message")
	}
}

// TestPersistReportInterruptedWritePreservesLastGood covers the
// "interrupted write" acceptance criterion at the persistReport call site:
// if the atomic write itself can't complete (here: the report directory
// loses its write bit mid-run, standing in for a disk-full/crash write
// failure), the previously persisted good report must survive untouched
// rather than being partially overwritten.
func TestPersistReportInterruptedWritePreservesLastGood(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod-based permission checks")
	}
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "last-report.json")
	svc := NewService(Deps{
		Cfg:            config.SelfMonitorConfig{Enabled: true, IntervalHours: 6},
		Health:         &stubHealth{Report: &health.Report{Score: health.ScoreGood}},
		LastReportPath: reportPath,
	})
	svc.tickAndLog(context.Background())

	good, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("seed persisted file missing: %v", err)
		panic("unreachable")
	}

	// fsutil.AtomicWrite's temp file must land next to the target to rename
	// atomically, so dropping the directory's write bit blocks the write
	// before it can touch the existing file at all.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	svc.deps.Health = &stubHealth{Report: &health.Report{Score: health.ScoreCritical}}
	svc.tickAndLog(context.Background())

	_ = os.Chmod(dir, 0o755)
	after, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("persisted file missing after interrupted write: %v", err)
		panic("unreachable")
	}
	if !bytes.Equal(after, good) {
		t.Errorf("last-report.json changed after an interrupted write; want the previous good report preserved\nbefore=%s\nafter=%s", good, after)
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
		panic("unreachable")
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
		panic("unreachable")
	}

	r := DiskHealthReader{Path: path}
	got, err := r.LatestReport()
	if err != nil {
		t.Fatalf("LatestReport: %v", err)
		panic("unreachable")
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
