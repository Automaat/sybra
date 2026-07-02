package promptlab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/evaluation"
)

func writeReport(t *testing.T, dir string, report evaluation.Report) string {
	t.Helper()
	path := filepath.Join(dir, "evaluation-report.json")
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return path
}

func TestRunEmptyReport(t *testing.T) {
	dir := t.TempDir()

	// Missing report file: treated as "no evidence yet", not an error.
	missing := filepath.Join(dir, "does-not-exist.json")
	result, err := Run(context.Background(), Options{ReportPath: missing, OutputDir: filepath.Join(dir, "out")})
	if err != nil {
		t.Fatalf("Run with missing report: %v", err)
	}
	if len(result.Proposals) != 0 || len(result.WeakSubjects) != 0 {
		t.Fatalf("expected empty result for missing report, got %+v", result)
	}

	// Malformed report file: genuine error, nothing written.
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write bad report: %v", err)
	}
	outDir := filepath.Join(dir, "out-bad")
	if _, err := Run(context.Background(), Options{ReportPath: badPath, OutputDir: outDir}); err == nil {
		t.Fatal("expected error for malformed report")
	}
	if _, err := os.Stat(filepath.Join(outDir, "last-run.json")); !os.IsNotExist(err) {
		t.Fatalf("malformed report must write nothing, stat err = %v", err)
	}
}

func TestRunFilesHumanRequired(t *testing.T) {
	dir := t.TempDir()
	report := evaluation.Report{
		Overall: evaluation.Scorecard{FailureRate: 0.10},
		ByRole:  []evaluation.Breakdown{{Key: "implementation", Runs: 20, FailureRate: 0.50}},
	}
	path := writeReport(t, dir, report)
	outDir := filepath.Join(dir, "out")

	result, err := Run(context.Background(), Options{
		ReportPath:    path,
		OutputDir:     outDir,
		MinSamples:    5,
		MinEffectSize: 0.1,
		Now:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Proposals) == 0 {
		t.Fatal("expected at least one proposal")
	}
	for _, p := range result.Proposals {
		if !p.RequiresHumanApproval {
			t.Fatalf("proposal %s: default stub evaluator must force human approval", p.ID)
		}
		if p.Offline.Verdict != VerdictNoVerdict {
			t.Fatalf("proposal %s: verdict = %q, want no-verdict", p.ID, p.Offline.Verdict)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, "last-run.json")); err != nil {
		t.Fatalf("expected last-run.json to be written: %v", err)
	}
}

func TestRunCapsProposalsAndLogs(t *testing.T) {
	dir := t.TempDir()
	report := evaluation.Report{
		Overall: evaluation.Scorecard{FailureRate: 0.0},
		ByRole: []evaluation.Breakdown{
			{Key: "implementation", Runs: 20, FailureRate: 0.60},
			{Key: "review", Runs: 20, FailureRate: 0.50},
			{Key: "fix-review", Runs: 20, FailureRate: 0.40},
		},
	}
	path := writeReport(t, dir, report)

	result, err := Run(context.Background(), Options{
		ReportPath:    path,
		OutputDir:     filepath.Join(dir, "out"),
		MinSamples:    5,
		MinEffectSize: 0.1,
		MaxProposals:  1,
		Now:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("len(result.Proposals) = %d, want 1", len(result.Proposals))
	}
	if result.Dropped == 0 {
		t.Fatal("expected Dropped > 0 when cap is hit")
	}
	if result.Proposals[0].Subject.Role != "implementation" {
		t.Fatalf("kept proposal role = %q, want implementation (highest effect size)", result.Proposals[0].Subject.Role)
	}
}
