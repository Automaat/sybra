package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestEnsureBareCloneHealthy_RepairsBeforeFetch(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}

	plantBadRef(t, bare, "refs/heads/dead-sibling")
	if err := CheckBareCloneHealth(context.Background(), bare); err == nil {
		t.Fatal("CheckBareCloneHealth succeeded on a clone with a poisoned ref")
	}

	report, err := EnsureBareCloneHealthy(context.Background(), bare, "")
	if err != nil {
		t.Fatalf("EnsureBareCloneHealthy: %v", err)
	}
	if len(report.QuarantinedRefs) == 0 {
		t.Fatalf("QuarantinedRefs = %v, want the poisoned ref recorded", report.QuarantinedRefs)
	}
	if err := CheckBareCloneHealth(context.Background(), bare); err != nil {
		t.Fatalf("clone is still unhealthy after repair: %v", err)
	}
}

func TestFetchOrigin_ChecksHealthBeforeFreshTTLSkip(t *testing.T) {
	origTTL := FetchTTL
	FetchTTL = time.Hour
	t.Cleanup(func() { FetchTTL = origTTL })

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("initial FetchOrigin: %v", err)
	}
	plantBadRef(t, bare, "refs/heads/dead-after-fetch")

	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("FetchOrigin should repair even while fetch TTL is fresh: %v", err)
	}
	if _, err := outputBare(context.Background(), bare, "rev-parse", "--verify", "refs/heads/dead-after-fetch"); err == nil {
		t.Fatal("dead-after-fetch ref should have been removed despite fresh fetch TTL")
	}
}

func TestEnsureBareCloneHealthy_RebuildsCorruptWorktreeIndex(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("fetch origin: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := CreateWorktree(context.Background(), bare, wtPath, "index-task", "origin/"+branch); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	indexOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "--git-path", "index").Output()
	if err != nil {
		t.Fatal(err)
	}
	indexPath := strings.TrimSpace(string(indexOut))
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(wtPath, indexPath)
	}
	if err := os.WriteFile(indexPath, []byte("not a git index"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unreadable worktree index is CheckWorktreeIndexes' business, not the
	// object-database gate's: a broken index makes one worktree unusable, it
	// does not make the clone unusable for every other task.
	if err := CheckWorktreeIndexes(context.Background(), bare); err == nil {
		t.Fatal("CheckWorktreeIndexes succeeded with a corrupt worktree index")
	}
	if err := CheckBareCloneHealth(context.Background(), bare); err != nil {
		t.Fatalf("one worktree's broken index failed the clone-wide health gate: %v", err)
	}

	report, err := EnsureBareCloneHealthy(context.Background(), bare, "")
	if err != nil {
		t.Fatalf("EnsureBareCloneHealthy: %v", err)
	}
	if len(report.RebuiltWorktreeIndexes) == 0 {
		t.Fatalf("RebuiltWorktreeIndexes = %v, want the corrupt index recorded", report.RebuiltWorktreeIndexes)
	}
	if err := CheckBareCloneHealth(context.Background(), bare); err != nil {
		t.Fatalf("clone is still unhealthy after index rebuild: %v", err)
	}
	if out, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("worktree still cannot read rebuilt index: %v: %s", err, out)
	}
}

func TestRepairBareClone_DeletesRefStillCheckedOutByDeadWorktree(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("fetch origin: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := CreateWorktree(context.Background(), bare, wtPath, "task-branch", "origin/"+branch); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", wtPath, "config", "user.email", "test@test.com"},
		{"-C", wtPath, "config", "user.name", "Test"},
		{"-C", wtPath, "add", "."},
		{"-C", wtPath, "commit", "-m", "extra"},
	} {
		if out, cmdErr := exec.Command("git", args...).CombinedOutput(); cmdErr != nil {
			t.Fatalf("git %v: %v: %s", args, cmdErr, out)
		}
	}
	headOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	headSHA := strings.TrimSpace(string(headOut))
	objPath := filepath.Join(bare, "objects", headSHA[:2], headSHA[2:])
	if err := os.Remove(objPath); err != nil {
		t.Fatalf("corrupt commit object: %v", err)
	}

	origDir := QuarantineDir
	QuarantineDir = t.TempDir()
	t.Cleanup(func() { QuarantineDir = origDir })

	if _, err := RepairBareClone(context.Background(), bare, ""); err != nil {
		t.Fatalf("RepairBareClone: %v", err)
	}

	if _, err := outputBare(context.Background(), bare, "rev-parse", "--verify", "refs/heads/task-branch"); err == nil {
		t.Fatal("dead task-branch ref should have been deleted, but a live worktree HEAD still pinned it")
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

// Reproduces the production wedge: a worktree HEAD reflog naming an object the
// bare clone no longer has. Unscoped `git fsck` exits non-zero on this forever,
// which parked tasks human-required with "bare clone health check failed" that
// no retry could clear, even though every ref-reachable object was present.
func TestCheckBareCloneHealth_IgnoresStaleWorktreeReflog(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	if err := FetchOrigin(context.Background(), bare); err != nil {
		t.Fatalf("fetch origin: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "wt")
	if err := CreateWorktree(context.Background(), bare, wtPath, "reflog-task", "origin/"+branch); err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	// Commit in the worktree so its HEAD reflog names the new commit, then
	// drop the branch and the object so nothing reachable references it —
	// exactly the state a pruned bare clone leaves behind.
	if err := os.WriteFile(filepath.Join(wtPath, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", wtPath, "config", "user.email", "test@test.com"},
		{"git", "-C", wtPath, "config", "user.name", "Test"},
		{"git", "-C", wtPath, "add", "."},
		{"git", "-C", wtPath, "commit", "-m", "scratch"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	shaOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(shaOut))
	if out, err := exec.Command("git", "-C", wtPath, "reset", "--hard", "HEAD~1").CombinedOutput(); err != nil {
		t.Fatalf("reset: %v: %s", err, out)
	}
	loose := filepath.Join(bare, "objects", sha[:2], sha[2:])
	if _, statErr := os.Stat(loose); statErr != nil {
		t.Skipf("scratch commit is not a loose object: %v", statErr)
	}
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}

	if err := runBare(context.Background(), bare, "fsck", "--no-dangling"); err == nil {
		t.Fatal("unscoped fsck passed; this test no longer reproduces the wedge it pins")
	}
	if err := CheckBareCloneHealth(context.Background(), bare); err != nil {
		t.Fatalf("stale worktree reflog failed the health gate: %v", err)
	}
}

// Real object-database damage must still fail the gate, or the fix above would
// have traded a false positive for a false negative.
func TestCheckBareCloneHealth_StillDetectsMissingReachableObject(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("clone bare: %v", err)
	}
	if err := CheckBareCloneHealth(context.Background(), bare); err != nil {
		t.Fatalf("freshly cloned bare repo is unhealthy: %v", err)
	}

	headSHA, err := outputBare(context.Background(), bare, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	sha := strings.TrimSpace(headSHA)
	loose := filepath.Join(bare, "objects", sha[:2], sha[2:])
	if _, statErr := os.Stat(loose); statErr != nil {
		t.Skipf("HEAD commit is packed, not loose: %v", statErr)
	}
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}

	if err := CheckBareCloneHealth(context.Background(), bare); err == nil {
		t.Fatal("health gate passed with the HEAD commit object deleted")
	}
}
