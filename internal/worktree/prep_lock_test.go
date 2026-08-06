package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func TestLockPath_SecondCallerIsRefused(t *testing.T) {
	m := &Manager{dir: t.TempDir()}
	path := filepath.Join(m.dir, "wt")

	release, err := m.lockPath(path)
	if err != nil {
		t.Fatalf("first lockPath: %v", err)
	}
	if _, err := m.lockPath(path); !errors.Is(err, ErrPreparationInFlight) {
		t.Fatalf("second lockPath err = %v, want ErrPreparationInFlight", err)
	}

	release()
	release2, err := m.lockPath(path)
	if err != nil {
		t.Fatalf("lockPath after release: %v", err)
	}
	release2()
	if n := len(m.paths.held); n != 0 {
		t.Errorf("held entries after release = %d, want 0", n)
	}
}

// TestLockPath_DistinctPathsAreIndependent pins that the exclusion is
// per-directory: two best-of-N attempts of the same task prepare concurrently
// by design and must not serialize against each other.
func TestLockPath_DistinctPathsAreIndependent(t *testing.T) {
	m := &Manager{dir: t.TempDir()}

	releaseA, err := m.lockPath(filepath.Join(m.dir, "a"))
	if err != nil {
		t.Fatalf("lock a: %v", err)
	}
	defer releaseA()
	releaseB, err := m.lockPath(filepath.Join(m.dir, "b"))
	if err != nil {
		t.Fatalf("lock b: %v", err)
	}
	releaseB()
}

// TestLockPath_NormalizesPath proves the key is the cleaned path, so the same
// directory reached by two spellings is still one lock.
func TestLockPath_NormalizesPath(t *testing.T) {
	m := &Manager{dir: t.TempDir()}

	release, err := m.lockPath(filepath.Join(m.dir, "wt"))
	if err != nil {
		t.Fatalf("lockPath: %v", err)
	}
	defer release()

	if _, err := m.lockPath(filepath.Join(m.dir, "sub", "..", "wt")); !errors.Is(err, ErrPreparationInFlight) {
		t.Fatalf("unnormalized path err = %v, want ErrPreparationInFlight", err)
	}
}

// TestPrepareForTask_RefusesSecondConcurrentPreparation is the acceptance test
// for #3114: a preparation that outlives the dispatch claim's age-based
// release must not permit a second one on the same worktree. It holds the
// first preparation open inside its setup batch — the same place a stalled
// fetch/push would hold it — and asserts the second caller is refused rather
// than allowed to run `git worktree add`/rebase over the top.
func TestPrepareForTask_RefusesSecondConcurrentPreparation(t *testing.T) {
	signals := t.TempDir()
	started := filepath.Join(signals, "started")
	unblock := filepath.Join(signals, "unblock")

	h := prepareHarness(t, []string{
		fmt.Sprintf("touch %s; while [ ! -f %s ]; do sleep 0.02; done", started, unblock),
	}, 60*time.Second)

	tk, err := h.tasks.Store().Create("concurrent prepare", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, firstErr = h.m.PrepareForTask(context.Background(), tk, nil)
	}()
	// Unblocks and drains the incumbent however the test exits — a t.Fatalf
	// below would otherwise leave the setup command spinning.
	t.Cleanup(func() {
		_ = os.WriteFile(unblock, nil, 0o600)
		<-done
	})

	waitForFile(t, started)

	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); !errors.Is(err, ErrPreparationInFlight) {
		t.Fatalf("second PrepareForTask err = %v, want ErrPreparationInFlight", err)
	}

	if err := os.WriteFile(unblock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
	if firstErr != nil {
		t.Fatalf("first PrepareForTask: %v", firstErr)
	}

	// The path is released once the incumbent finishes, so the task is not
	// wedged: the retry a dispatcher makes after the transient refusal works.
	if _, err := h.m.PrepareForTask(context.Background(), tk, nil); err != nil {
		t.Fatalf("PrepareForTask after release: %v", err)
	}
}

// TestLockedEntryPointsRefuseHeldPath sweeps the mutating entry points a
// dispatcher can reach while another one owns the path. Each must refuse
// rather than run its git operations over the top: ResetForRetry aborts an
// in-progress rebase and hard-resets, RecreateFromBase deletes the directory
// and its branch, and both are reached from re-dispatch paths that fire on
// precisely the hung run that provokes the stale claim release.
func TestLockedEntryPointsRefuseHeldPath(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("held path", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	release, err := h.m.lockPath(h.m.PathFor(tk))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, _, err := h.m.ResetForRetry(context.Background(), tk, "", "HEAD"); !errors.Is(err, ErrPreparationInFlight) {
		t.Errorf("ResetForRetry err = %v, want ErrPreparationInFlight", err)
	}
	if err := h.m.RecreateFromBase(context.Background(), tk); !errors.Is(err, ErrPreparationInFlight) {
		t.Errorf("RecreateFromBase err = %v, want ErrPreparationInFlight", err)
	}
	if _, err := h.m.PrepareForReview(context.Background(), tk); !errors.Is(err, ErrPreparationInFlight) {
		t.Errorf("PrepareForReview err = %v, want ErrPreparationInFlight", err)
	}
	if _, err := h.m.PrepareForBranchFix(context.Background(), tk); !errors.Is(err, ErrPreparationInFlight) {
		t.Errorf("PrepareForBranchFix err = %v, want ErrPreparationInFlight", err)
	}
	if _, err := h.m.PrepareForBranchConflict(context.Background(), tk); !errors.Is(err, ErrPreparationInFlight) {
		t.Errorf("PrepareForBranchConflict err = %v, want ErrPreparationInFlight", err)
	}
	if _, err := h.m.PrepareForFix(context.Background(), tk, 1); !errors.Is(err, ErrPreparationInFlight) {
		t.Errorf("PrepareForFix err = %v, want ErrPreparationInFlight", err)
	}
	if err := h.m.PruneMissingWorktree(context.Background(), h.proj.ClonePath, h.m.PathFor(tk)); !errors.Is(err, ErrPreparationInFlight) {
		t.Errorf("PruneMissingWorktree err = %v, want ErrPreparationInFlight", err)
	}
	if _, _, err := h.m.PrepareAttempt(context.Background(), tk, "att1"); err != nil {
		t.Errorf("PrepareAttempt on a different path must not be refused: %v", err)
	}
}

// TestResetForRetry_ResetsWhenPathIsFree is the positive half: the refusal
// above must come from the lock, not from ResetForRetry being broken.
func TestResetForRetry_ResetsWhenPathIsFree(t *testing.T) {
	h := prepareHarness(t, nil, 30*time.Second)

	tk, err := h.tasks.Store().Create("free path", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(h.proj.ID)}); err != nil {
		t.Fatal(err)
	}
	tk, err = h.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	// No worktree yet: nothing to reset, and that is not an error. The
	// resolved path still comes back, since callers log it rather than the
	// empty dir argument they passed.
	target, reset, err := h.m.ResetForRetry(context.Background(), tk, "", "HEAD")
	if err != nil {
		t.Fatalf("ResetForRetry with no worktree: %v", err)
	}
	if reset {
		t.Error("reset = true with no worktree on disk, want false")
	}
	if want := h.m.PathFor(tk); target != want {
		t.Errorf("resolved path = %q, want %q", target, want)
	}

	dir, err := h.m.PrepareForTask(context.Background(), tk, nil)
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, "stray.txt")
	if err := os.WriteFile(stray, []byte("partial work"), 0o600); err != nil {
		t.Fatal(err)
	}

	target, reset, err = h.m.ResetForRetry(context.Background(), tk, "", "HEAD")
	if err != nil {
		t.Fatalf("ResetForRetry: %v", err)
	}
	if !reset {
		t.Error("reset = false on an existing worktree, want true")
	}
	if target != dir {
		t.Errorf("resolved path = %q, want the prepared worktree %q", target, dir)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Errorf("stray file survived the reset: %v", err)
	}
}

// TestCleanup_SkipsHeldPathAndSweepsItLater covers the deletion half. Remove
// declines a directory a preparation owns, and CleanupOrphaned — the sweep
// Remove defers to — must both take the same lock and actually be able to reap
// the deferred task, which for a cancelled one means looking past `done`.
func TestCleanup_SkipsHeldPathAndSweepsItLater(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	m := New(Config{WorktreesDir: dir, Tasks: tasks, Logger: discardLogger()})

	tk, err := store.Create("cancelled task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusCancelled)}); err != nil {
		t.Fatal(err)
	}
	wtPath := filepath.Join(dir, tk.DirName())
	makePushedGitDir(t, wtPath)

	release, err := m.lockPath(wtPath)
	if err != nil {
		t.Fatal(err)
	}

	m.Remove(context.Background(), tk.ID)
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("Remove deleted a worktree a preparation owns: %v", err)
	}
	m.CleanupOrphaned(context.Background())
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("CleanupOrphaned deleted a worktree a preparation owns: %v", err)
	}

	release()
	m.CleanupOrphaned(context.Background())
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("cancelled task's worktree survived the sweep once the path was free: %v", err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
