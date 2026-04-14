package selfmonitor

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLedgerAppendAndLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	e1 := LedgerEntry{
		Fingerprint: "stuck_task:task-1",
		Category:    "stuck_task",
		TaskID:      "task-1",
		Verdict:     VerdictConfirmed,
	}
	if err := l.Append(e1); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if l.Len() != 1 {
		t.Errorf("Len = %d, want 1", l.Len())
	}

	got, ok := l.Latest("stuck_task:task-1")
	if !ok {
		t.Fatal("Latest returned not found")
	}
	if got.TaskID != "task-1" || got.Verdict != VerdictConfirmed {
		t.Errorf("Latest = %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt not stamped")
	}
}

func TestLedgerReplayAcrossOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	for i := range 3 {
		if err := l1.Append(LedgerEntry{
			Fingerprint: "cost_outlier:task-1",
			Verdict:     VerdictFalsePositive,
			Summary:     []string{"a", "b", "c"}[i],
		}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	// Re-open and ensure replay reconstructs the index.
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if l2.Len() != 3 {
		t.Errorf("replayed Len = %d, want 3", l2.Len())
	}
	got, ok := l2.Latest("cost_outlier:task-1")
	if !ok {
		t.Fatal("Latest after replay missing")
	}
	if got.Summary != "c" {
		t.Errorf("Latest.Summary = %q, want \"c\"", got.Summary)
	}
}

func TestLedgerShouldAutoSuppress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	fp := "cost_outlier:task-xyz"
	now := time.Now().UTC()
	for range 3 {
		if err := l.Append(LedgerEntry{
			Fingerprint: fp,
			Verdict:     VerdictFalsePositive,
			CreatedAt:   now,
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if !l.ShouldAutoSuppress(fp, 24*time.Hour, 3) {
		t.Error("ShouldAutoSuppress = false, want true after 3 false positives")
	}
	if l.ShouldAutoSuppress(fp, 24*time.Hour, 4) {
		t.Error("ShouldAutoSuppress at threshold 4 = true, want false")
	}

	// Stale entries should be ignored when the window is narrow.
	other := "cost_outlier:task-old"
	if err := l.Append(LedgerEntry{
		Fingerprint: other,
		Verdict:     VerdictFalsePositive,
		CreatedAt:   now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("Append stale: %v", err)
	}
	if l.ShouldAutoSuppress(other, 1*time.Hour, 1) {
		t.Error("stale entry should fall outside narrow window")
	}
}

func TestLedgerOpenIssues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := l.Append(LedgerEntry{
		Fingerprint: "a",
		IssueNumber: 10,
		IssueState:  "open",
	}); err != nil {
		t.Fatalf("Append a: %v", err)
	}
	if err := l.Append(LedgerEntry{
		Fingerprint: "b",
		IssueNumber: 20,
		IssueState:  "closed",
	}); err != nil {
		t.Fatalf("Append b: %v", err)
	}
	if err := l.Append(LedgerEntry{
		Fingerprint: "c",
	}); err != nil {
		t.Fatalf("Append c: %v", err)
	}

	open := l.OpenIssues()
	if len(open) != 1 {
		t.Fatalf("OpenIssues = %d, want 1", len(open))
	}
	if open[0].Fingerprint != "a" || open[0].IssueNumber != 10 {
		t.Errorf("OpenIssues[0] = %+v", open[0])
	}
}

func TestLedgerActionsInWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	now := time.Now().UTC()
	rows := []LedgerEntry{
		{Fingerprint: "a", Action: "status_update", CreatedAt: now},
		{Fingerprint: "b", Action: "status_update", CreatedAt: now.Add(-2 * time.Hour)},
		{Fingerprint: "c", Action: "", CreatedAt: now},                               // skipped — no action
		{Fingerprint: "d", Action: "config_pr", CreatedAt: now.Add(-48 * time.Hour)}, // out of window
	}
	for _, r := range rows {
		if err := l.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := l.ActionsInWindow(24 * time.Hour); got != 2 {
		t.Errorf("ActionsInWindow = %d, want 2", got)
	}
}

func TestLedgerAppendEmptyFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Append(LedgerEntry{}); err == nil {
		t.Error("Append empty fingerprint = nil, want error")
	}
}
