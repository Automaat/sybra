package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func plantBadRef(t *testing.T, bare, ref string) {
	t.Helper()
	path := filepath.Join(bare, filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("f", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFetchOrigin_RepairsPoisonedSiblingRef(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	plantBadRef(t, bare, "refs/heads/dead-sibling")

	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("FetchOrigin should repair the poisoned sibling ref and succeed, got: %v", err)
	}

	trackingRef := "refs/remotes/origin/" + branch
	if _, err := outputBare(context.Background(), bare, "rev-parse", "--verify", trackingRef); err != nil {
		t.Fatalf("good branch %s should still be fetchable after repair: %v", trackingRef, err)
	}

	if _, err := outputBare(context.Background(), bare, "rev-parse", "--verify", "refs/heads/dead-sibling"); err == nil {
		t.Fatal("dead-sibling ref should have been removed by the repair pass")
	}
}

func TestRepairBareClone_QuarantinesRefsAndWorktreeHead(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}

	origDir := QuarantineDir
	quarantineDir := t.TempDir()
	QuarantineDir = quarantineDir
	t.Cleanup(func() { QuarantineDir = origDir })

	plantBadRef(t, bare, "refs/heads/dead-sibling")
	plantBadRef(t, bare, "worktrees/stale-wt/HEAD")

	report, err := RepairBareClone(context.Background(), bare, "")
	if err != nil {
		t.Fatalf("RepairBareClone: %v", err)
	}

	if len(report.QuarantinedRefs) != 2 {
		t.Fatalf("QuarantinedRefs = %v, want 2 entries (dead-sibling + stale worktree HEAD)", report.QuarantinedRefs)
	}
	if !report.PrunedWorktrees {
		t.Fatal("PrunedWorktrees should be true when a broken ref was found")
	}

	entries, err := os.ReadDir(quarantineDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected a quarantine log file, got entries=%v err=%v", entries, err)
	}
	logBytes, err := os.ReadFile(filepath.Join(quarantineDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "refs/heads/dead-sibling") {
		t.Fatalf("quarantine log = %q, want it to mention the pruned ref instead of silently dropping it", log)
	}
	if !strings.Contains(log, strings.Repeat("f", 40)) {
		t.Fatalf("quarantine log = %q, want it to record the bad SHA", log)
	}
}

func TestRepairBareClone_NoOpOnCleanClone(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}

	report, err := RepairBareClone(context.Background(), bare, "")
	if err != nil {
		t.Fatalf("RepairBareClone: %v", err)
	}
	if len(report.QuarantinedRefs) != 0 {
		t.Fatalf("QuarantinedRefs = %v, want none on a clean clone", report.QuarantinedRefs)
	}
	if report.PrunedWorktrees {
		t.Fatal("PrunedWorktrees should be false when nothing was broken")
	}
}

func TestIsBadRefError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"fatal: bad object refs/heads/foo", true},
		{"fatal: unable to read sha1 file", true},
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		var err error
		if tc.msg != "" {
			err = testError(tc.msg)
		}
		if got := IsBadRefError(err); got != tc.want {
			t.Errorf("IsBadRefError(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

type testError string

func (e testError) Error() string { return string(e) }
