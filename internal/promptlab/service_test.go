package promptlab

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/stats"
)

func TestRunNoRecords(t *testing.T) {
	dir := t.TempDir()

	result, err := Run(context.Background(), Options{OutputDir: filepath.Join(dir, "out")})
	if err != nil {
		t.Fatalf("Run with no records: %v", err)
	}
	if len(result.Proposals) != 0 || len(result.WeakSubjects) != 0 {
		t.Fatalf("expected empty result for no records, got %+v", result)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "last-run.json")); err != nil {
		t.Fatalf("expected last-run.json to still be written: %v", err)
	}
}

func TestRunFilesHumanRequired(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	var records []stats.RunRecord
	records = append(records, weakRoleRecords("implementation", 10, 20)...) // 0.50 failure rate
	records = append(records, weakRoleRecords("review", 0, 20)...)          // 0.00, dilutes baseline to 0.25

	result, err := Run(context.Background(), Options{
		Records:       records,
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
	var records []stats.RunRecord
	records = append(records, weakRoleRecords("implementation", 12, 20)...) // 0.60
	records = append(records, weakRoleRecords("review", 10, 20)...)         // 0.50
	records = append(records, weakRoleRecords("fix-review", 8, 20)...)      // 0.40
	records = append(records, weakRoleRecords("docs", 0, 100)...)           // dilutes baseline to 0.1875

	result, err := Run(context.Background(), Options{
		Records:       records,
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

// weakRoleRecords returns `runs` records for role with `fails` of them failed
// and the rest passing, so tests can build up a role's failure rate without
// hand-listing each record.
func weakRoleRecords(role string, fails, runs int) []stats.RunRecord {
	out := make([]stats.RunRecord, runs)
	for i := range out {
		outcome := stats.OutcomeCompleted
		if i < fails {
			outcome = stats.OutcomeFailed
		}
		out[i] = stats.RunRecord{Role: role, Outcome: outcome, TaskID: role + string(rune('a'+i))}
	}
	return out
}
