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
