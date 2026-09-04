package harnessevolution

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/selfmonitor"
)

func writeSelfMonitorReport(t *testing.T, dir string, r selfmonitor.Report) string {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	path := filepath.Join(dir, "last-report.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return path
}

func actionableFinding(taskID string, at time.Time) selfmonitor.InvestigatedFinding {
	return selfmonitor.InvestigatedFinding{
		Finding: health.Finding{
			Category:   health.CatAgentRetryLoop,
			TaskID:     taskID,
			AgentID:    "agent-" + taskID,
			Role:       "implementation",
			Evidence:   map[string]any{"step": "implement"},
			DetectedAt: at,
		},
		Fingerprint: "fp-" + taskID,
		Verdict:     selfmonitor.Verdict{Classification: selfmonitor.VerdictConfirmed},
	}
}

func TestRun_SelfMonitorDisabledWithFreshReportStillConsumesIt(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	// Disabling self-monitor to stop background spend must not throw away a
	// fresh report it already wrote — Run consumes it and produces proposals.
	path := writeSelfMonitorReport(t, dir, selfmonitor.Report{
		GeneratedAt: now,
		State:       selfmonitor.StateHealthy,
		Findings: []selfmonitor.InvestigatedFinding{
			actionableFinding("t1", now),
			actionableFinding("t2", now),
		},
	})

	result, err := Run(context.Background(), Options{
		ReportPath:         path,
		SelfMonitorEnabled: false,
		MaxReportAge:       48 * time.Hour,
		MinClusterSize:     2,
		Now:                now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != selfmonitor.StateHealthy {
		t.Errorf("State = %q, want %q (a fresh report is usable regardless of the disabled flag)", result.State, selfmonitor.StateHealthy)
	}
	if len(result.Clusters) != 1 {
		t.Fatalf("Clusters = %d, want 1", len(result.Clusters))
	}
}

func TestRun_SelfMonitorDisabledWithNoReportReturnsDisabledOutcome(t *testing.T) {
	dir := t.TempDir()
	// No report on disk and self-monitor disabled: none will ever be
	// produced, so Run reports StateDisabled explicitly.
	result, err := Run(context.Background(), Options{
		ReportPath:         filepath.Join(dir, "last-report.json"),
		SelfMonitorEnabled: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != selfmonitor.StateDisabled {
		t.Errorf("State = %q, want %q", result.State, selfmonitor.StateDisabled)
	}
	if result.Reason == "" {
		t.Error("Reason is empty, want an explanation of the disabled dependency")
	}
	if len(result.Proposals) != 0 || len(result.Clusters) != 0 {
		t.Errorf("expected no proposals/clusters for a disabled dependency with no report, got %+v", result)
	}
}

func TestRun_MissingReportReturnsStaleDegradedOutcome(t *testing.T) {
	dir := t.TempDir()

	result, err := Run(context.Background(), Options{
		ReportPath:         filepath.Join(dir, "does-not-exist.json"),
		SelfMonitorEnabled: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != selfmonitor.StateStale {
		t.Errorf("State = %q, want %q", result.State, selfmonitor.StateStale)
	}
	if !strings.Contains(result.Reason, "no self-monitor report found") {
		t.Errorf("Reason = %q, want it to mention the missing report", result.Reason)
	}
}

func TestRun_StaleReportReturnsDegradedOutcome(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().UTC().Add(-72 * time.Hour)
	path := writeSelfMonitorReport(t, dir, selfmonitor.Report{
		GeneratedAt: old,
		State:       selfmonitor.StateHealthy,
		Findings:    []selfmonitor.InvestigatedFinding{actionableFinding("t1", old)},
	})

	result, err := Run(context.Background(), Options{
		ReportPath:         path,
		SelfMonitorEnabled: true,
		MaxReportAge:       48 * time.Hour,
		Now:                time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != selfmonitor.StateStale {
		t.Errorf("State = %q, want %q", result.State, selfmonitor.StateStale)
	}
	if len(result.Proposals) != 0 {
		t.Errorf("expected no proposals from a stale report, got %+v", result.Proposals)
	}
}

func TestRun_LongIntervalKeepsOnScheduleReportHealthy(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	// 60h old exceeds the 48h default window but is within a 72h tick interval,
	// so a healthy on-schedule report must not be flagged stale.
	generated := now.Add(-60 * time.Hour)
	path := writeSelfMonitorReport(t, dir, selfmonitor.Report{
		GeneratedAt: generated,
		State:       selfmonitor.StateHealthy,
		Findings: []selfmonitor.InvestigatedFinding{
			actionableFinding("t1", generated),
			actionableFinding("t2", generated),
		},
	})

	result, err := Run(context.Background(), Options{
		ReportPath:          path,
		SelfMonitorEnabled:  true,
		SelfMonitorInterval: 72 * time.Hour,
		MinClusterSize:      2,
		Now:                 now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != selfmonitor.StateHealthy {
		t.Errorf("State = %q, want %q", result.State, selfmonitor.StateHealthy)
	}
	if len(result.Proposals) == 0 {
		t.Errorf("expected proposals from an on-schedule report, got none")
	}
}

func TestRun_HealthyReportProducesProposals(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := writeSelfMonitorReport(t, dir, selfmonitor.Report{
		GeneratedAt: now,
		State:       selfmonitor.StateHealthy,
		Findings: []selfmonitor.InvestigatedFinding{
			actionableFinding("t1", now),
			actionableFinding("t2", now),
		},
	})

	result, err := Run(context.Background(), Options{
		ReportPath:         path,
		SelfMonitorEnabled: true,
		MaxReportAge:       48 * time.Hour,
		MinClusterSize:     2,
		Now:                now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != selfmonitor.StateHealthy {
		t.Errorf("State = %q, want %q", result.State, selfmonitor.StateHealthy)
	}
	if result.Events != 2 {
		t.Errorf("Events = %d, want 2", result.Events)
	}
	if len(result.Clusters) != 1 {
		t.Fatalf("Clusters = %d, want 1", len(result.Clusters))
	}
}

func TestRun_PartialReportSurfacesPartialState(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	path := writeSelfMonitorReport(t, dir, selfmonitor.Report{
		GeneratedAt:      now,
		State:            selfmonitor.StatePartial,
		InputsTotal:      2,
		InputsAnalyzed:   1,
		TruncatedRecords: 1,
		Findings: []selfmonitor.InvestigatedFinding{
			actionableFinding("t1", now),
			actionableFinding("t2", now),
		},
	})

	result, err := Run(context.Background(), Options{
		ReportPath:         path,
		SelfMonitorEnabled: true,
		MaxReportAge:       48 * time.Hour,
		MinClusterSize:     2,
		Now:                now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != selfmonitor.StatePartial {
		t.Errorf("State = %q, want %q", result.State, selfmonitor.StatePartial)
	}
	if result.Reason == "" {
		t.Error("Reason is empty, want a coverage explanation")
	}
	// Partial coverage still yields whatever proposals the available data supports.
	if len(result.Clusters) != 1 {
		t.Errorf("Clusters = %d, want 1", len(result.Clusters))
	}
}

func TestRun_PersistsDegradedResultAtomically(t *testing.T) {
	dir := t.TempDir()
	outDir := t.TempDir()

	result, err := Run(context.Background(), Options{
		ReportPath:         filepath.Join(dir, "does-not-exist.json"),
		OutputDir:          outDir,
		SelfMonitorEnabled: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "last-run.json"))
	if err != nil {
		t.Fatalf("last-run.json missing: %v", err)
	}
	var back RunResult
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("last-run.json unparseable: %v", err)
	}
	if back.State != result.State {
		t.Errorf("persisted State = %q, want %q", back.State, result.State)
	}
}

// TestSaveRunResult_InterruptedWritePreservesLastGood mirrors the
// selfmonitor persistReport test: if SaveRunResult's atomic write can't
// complete, the previous last-run.json must survive untouched rather than
// being partially overwritten.
func TestSaveRunResult_InterruptedWritePreservesLastGood(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod-based permission checks")
	}
	outDir := t.TempDir()
	if err := SaveRunResult(outDir, RunResult{State: selfmonitor.StateHealthy, Events: 1}); err != nil {
		t.Fatalf("seed SaveRunResult: %v", err)
	}
	good, err := os.ReadFile(filepath.Join(outDir, "last-run.json"))
	if err != nil {
		t.Fatalf("seed file missing: %v", err)
	}

	if err := os.Chmod(outDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outDir, 0o755) })

	err = SaveRunResult(outDir, RunResult{State: selfmonitor.StateFailed, Events: 99})
	if err == nil {
		t.Skip("write did not fail on this platform/fs; test relies on directory permission semantics")
	}

	_ = os.Chmod(outDir, 0o755)
	after, readErr := os.ReadFile(filepath.Join(outDir, "last-run.json"))
	if readErr != nil {
		t.Fatalf("last-run.json missing after interrupted write: %v", readErr)
	}
	if !bytes.Equal(after, good) {
		t.Errorf("last-run.json changed after an interrupted write; want the previous good result preserved\nbefore=%s\nafter=%s", good, after)
	}
}
