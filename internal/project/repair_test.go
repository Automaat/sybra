package project

import (
	"context"
	"os"
	"os/exec"
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

func TestIsObjectCorruptionError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"fatal: unable to read a29bdeb434d874c9b1d8969c40c42161b03fafdc", true},
		{"remote: fatal: loose object b3c5a95f929a50feb06c275ac567cdb1b441d1e2 (stored in ./objects/b3/c5a95f...) is corrupt", true},
		{"error: broken link from tree abc123", true},
		{"missing blob abc123", true},
		{"fatal: bad object refs/heads/foo", false},
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		var err error
		if tc.msg != "" {
			err = testError(tc.msg)
		}
		if got := isObjectCorruptionError(err); got != tc.want {
			t.Errorf("isObjectCorruptionError(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// cloneBareNoHardlinks clones src into a fresh bare repo with independent
// object files (no --local hardlink sharing), so a test can corrupt an
// object in the bare clone without also corrupting src's copy — mirroring a
// real GitHub-backed bare clone, where origin's objects live on a different
// host entirely.
func cloneBareNoHardlinks(t *testing.T, src, dest string) {
	t.Helper()
	if out, err := exec.Command("git", "clone", "-q", "--bare", "--no-hardlinks", src, dest).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare --no-hardlinks: %v: %s", err, out)
	}
	if err := runBare(context.Background(), dest, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		t.Fatalf("config remote.origin.fetch: %v", err)
	}
}

// corruptBlob overwrites path's on-disk loose object bytes with garbage,
// simulating a shared bare clone object a concurrent task's repack/prune
// left unreadable.
func corruptBlob(t *testing.T, barePath, blobSHA string) {
	t.Helper()
	objPath := filepath.Join(barePath, "objects", blobSHA[:2], blobSHA[2:])
	if err := os.Chmod(objPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepairWorktreeObjectStore_RefetchesCorruptedObject(t *testing.T) {
	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	cloneBareNoHardlinks(t, src, bare)

	branch, err := DefaultBranch(context.Background(), bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if err := CreateWorktree(context.Background(), bare, wt, "feature", branch); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	blobSHA := strings.TrimSpace(runGit(t, wt, "rev-parse", "HEAD:README.md"))
	corruptBlob(t, bare, blobSHA)

	if _, err := exec.Command("git", "-C", bare, "cat-file", "-p", blobSHA).CombinedOutput(); err == nil {
		t.Fatal("expected corrupted blob to be unreadable before repair")
	}

	if err := repairWorktreeObjectStore(context.Background(), wt); err != nil {
		t.Fatalf("repairWorktreeObjectStore: %v", err)
	}

	out, err := exec.Command("git", "-C", bare, "cat-file", "-p", blobSHA).CombinedOutput()
	if err != nil {
		t.Fatalf("blob should be readable after repair, got: %v: %s", err, out)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
