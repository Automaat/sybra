package harnessevolution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/selfmonitor"
)

// writeReport persists a self-monitor report with the given GeneratedAt and a
// single actionable finding whose DetectedAt lands well inside any reasonable
// lookback window, returning the report path.
func writeReport(t *testing.T, generatedAt, detectedAt time.Time) string {
	t.Helper()
	report := selfmonitor.Report{
		GeneratedAt: generatedAt,
		Findings: []selfmonitor.InvestigatedFinding{
			findingWithVerdict("task-confirmed-a", selfmonitor.VerdictConfirmed, detectedAt),
			findingWithVerdict("task-confirmed-b", selfmonitor.VerdictConfirmed, detectedAt),
		},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	path := filepath.Join(t.TempDir(), "last-report.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return path
}

func TestRun_StaleReportDiscardsFindings(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	detectedAt := now.Add(-2 * time.Hour) // recent, inside lookback
	generatedAt := now.Add(-48 * time.Hour)
	path := writeReport(t, generatedAt, detectedAt)

	res, err := Run(context.Background(), Options{
		ReportPath:     path,
		Lookback:       168 * time.Hour,
		MinClusterSize: 1,
		MaxReportAge:   24 * time.Hour,
		Now:            now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.StaleReport {
		t.Fatalf("StaleReport = false, want true for a 48h-old report against a 24h max age")
	}
	if res.Events != 0 || len(res.Proposals) != 0 {
		t.Fatalf("stale report still produced events=%d proposals=%d, want 0/0", res.Events, len(res.Proposals))
	}
}

func TestRun_FreshReportKeepsFindings(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	detectedAt := now.Add(-2 * time.Hour)
	generatedAt := now.Add(-1 * time.Hour) // fresh
	path := writeReport(t, generatedAt, detectedAt)

	res, err := Run(context.Background(), Options{
		ReportPath:     path,
		Lookback:       168 * time.Hour,
		MinClusterSize: 1,
		MaxReportAge:   24 * time.Hour,
		Now:            now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StaleReport {
		t.Fatalf("StaleReport = true, want false for a 1h-old report")
	}
	if res.Events == 0 {
		t.Fatalf("fresh report produced 0 events, want the actionable findings to survive")
	}
}

func TestRun_MaxReportAgeDisabledKeepsStaleFindings(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	detectedAt := now.Add(-2 * time.Hour)
	generatedAt := now.Add(-500 * time.Hour) // very stale
	path := writeReport(t, generatedAt, detectedAt)

	res, err := Run(context.Background(), Options{
		ReportPath:     path,
		Lookback:       168 * time.Hour,
		MinClusterSize: 1,
		MaxReportAge:   0, // guard disabled
		Now:            now,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StaleReport {
		t.Fatalf("StaleReport = true with the guard disabled, want false")
	}
	if res.Events == 0 {
		t.Fatalf("guard disabled but events=0, want stale findings to pass through")
	}
}
