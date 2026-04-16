package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/selfmonitor"
)

// setupStoreWithHealth stages a health-report.json under SYBRA_HOME so
// cmdSelfmonitorInvestigate has something to read through DiskHealthReader.
func setupStoreWithHealth(t *testing.T, findings []health.Finding) string {
	t.Helper()
	home := setupStore(t)

	rep := health.Report{
		Score:    health.ScoreWarning,
		Findings: findings,
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal health report: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "health-report.json"), data, 0o644); err != nil {
		t.Fatalf("write health report: %v", err)
	}
	return home
}

func TestSelfmonitorScanMissingReport(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "selfmonitor", "scan")
	if code == 0 {
		t.Error("expected non-zero exit when no persisted report exists")
	}
}

func TestSelfmonitorScanPersistedReport(t *testing.T) {
	home := setupStore(t)
	if err := os.MkdirAll(filepath.Join(home, "selfmonitor"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rep := selfmonitor.Report{
		SchemaVersion: selfmonitor.ReportSchemaVersion,
		HealthScore:   health.ScoreCritical,
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "selfmonitor", "last-report.json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, out := runCLI(t, "--json", "selfmonitor", "scan")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var got selfmonitor.Report
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.HealthScore != health.ScoreCritical {
		t.Errorf("HealthScore = %q, want %q", got.HealthScore, health.ScoreCritical)
	}
}

func TestSelfmonitorInvestigateWithHealthReport(t *testing.T) {
	setupStoreWithHealth(t, []health.Finding{{
		Category:    health.CatCostOutlier,
		Severity:    health.SeverityWarning,
		Title:       "eval cost $0.65",
		Fingerprint: "cost_outlier:task-x",
		TaskID:      "task-x",
	}})

	code, out := runCLI(t, "--json", "selfmonitor", "investigate")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var rep selfmonitor.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(rep.Findings))
	}
	if rep.Findings[0].Verdict.Classification != selfmonitor.VerdictPending {
		t.Errorf("Verdict = %q, want pending", rep.Findings[0].Verdict.Classification)
	}
}

func TestSelfmonitorLedgerEmpty(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "--json", "selfmonitor", "ledger")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	// Empty ledger → empty slice or "null" from json.NewEncoder.
	trimmed := out
	if len(trimmed) == 0 {
		t.Fatal("empty output")
	}
}

func TestSelfmonitorUnknownSubcommand(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "selfmonitor", "bogus")
	if code == 0 {
		t.Error("expected non-zero exit for unknown selfmonitor subcommand")
	}
}

func TestSelfmonitorNoSubcommand(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "selfmonitor")
	if code == 0 {
		t.Error("expected non-zero exit when no subcommand is given")
	}
}
