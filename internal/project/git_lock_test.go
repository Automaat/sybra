package project

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWithBareRepoLock_SerializesSameClonePath(t *testing.T) {
	t.Parallel()
	bare := filepath.Join(t.TempDir(), "bare.git")

	var (
		mu          sync.Mutex
		inFlight    int
		maxInFlight int
		wg          sync.WaitGroup
	)
	enter := func() {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
	}
	leave := func() {
		mu.Lock()
		inFlight--
		mu.Unlock()
	}

	for range 8 {
		wg.Go(func() {
			_ = withBareRepoLock(bare, func() error {
				enter()
				time.Sleep(5 * time.Millisecond)
				leave()
				return nil
			})
		})
	}
	wg.Wait()

	if maxInFlight != 1 {
		t.Errorf("max concurrent holders of the same clone path = %d, want 1", maxInFlight)
	}
}

func TestWithBareRepoLock_DoesNotSerializeDifferentClonePaths(t *testing.T) {
	t.Parallel()
	bareA := filepath.Join(t.TempDir(), "a.git")
	bareB := filepath.Join(t.TempDir(), "b.git")

	release := make(chan struct{})
	started := make(chan struct{}, 2)

	var wg sync.WaitGroup
	for _, bare := range []string{bareA, bareB} {
		wg.Add(1)
		go func(bare string) {
			defer wg.Done()
			_ = withBareRepoLock(bare, func() error {
				started <- struct{}{}
				<-release
				return nil
			})
		}(bare)
	}

	// Both distinct-path holders must be able to enter concurrently — if a
	// single global lock were used instead of per-path, the second send to
	// started would block until the first releases, and this would time out.
	timeout := time.After(2 * time.Second)
	for range 2 {
		select {
		case <-started:
		case <-timeout:
			t.Fatal("timed out waiting for both distinct-path holders to enter concurrently")
		}
	}
	close(release)
	wg.Wait()
}

func TestWithLockRetry_RetriesOnLockContention(t *testing.T) {
	prevBackoffs, prevSleep := gitOpRetryBackoffs, gitOpRetrySleep
	t.Cleanup(func() { gitOpRetryBackoffs, gitOpRetrySleep = prevBackoffs, prevSleep })
	gitOpRetryBackoffs = []time.Duration{0, 0, 0}
	gitOpRetrySleep = func(time.Duration) {}

	var attempts int32
	err := withLockRetry(func() error {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			return fmt.Errorf("fatal: Unable to create '/repo/.git/index.lock': File exists.")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withLockRetry: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestWithLockRetry_DoesNotRetryNonLockErrors(t *testing.T) {
	prevBackoffs, prevSleep := gitOpRetryBackoffs, gitOpRetrySleep
	t.Cleanup(func() { gitOpRetryBackoffs, gitOpRetrySleep = prevBackoffs, prevSleep })
	gitOpRetryBackoffs = []time.Duration{0, 0, 0}
	gitOpRetrySleep = func(time.Duration) {
		t.Fatal("should not sleep/retry a non-lock error")
	}

	wantErr := errors.New("fatal: not a git repository")
	var attempts int32
	err := withLockRetry(func() error {
		atomic.AddInt32(&attempts, 1)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for non-lock errors)", attempts)
	}
}

func TestWithLockRetry_GivesUpAfterExhaustingBackoffs(t *testing.T) {
	prevBackoffs, prevSleep := gitOpRetryBackoffs, gitOpRetrySleep
	t.Cleanup(func() { gitOpRetryBackoffs, gitOpRetrySleep = prevBackoffs, prevSleep })
	gitOpRetryBackoffs = []time.Duration{0, 0}
	gitOpRetrySleep = func(time.Duration) {}

	var attempts int32
	lockErr := errors.New("fatal: Unable to create '.git/index.lock': File exists.")
	err := withLockRetry(func() error {
		atomic.AddInt32(&attempts, 1)
		return lockErr
	})
	if !errors.Is(err, lockErr) {
		t.Fatalf("err = %v, want %v", err, lockErr)
	}
	// One initial attempt plus one retry per configured backoff.
	if want := int32(1 + len(gitOpRetryBackoffs)); attempts != want {
		t.Errorf("attempts = %d, want %d", attempts, want)
	}
}

// TestCreateWorktree_ConcurrentDistinctPathsSucceed exercises the fix's core
// scenario: several task worktrees prepared concurrently off one shared bare
// clone. Before the per-clone-path mutex, concurrent `git worktree add`
// invocations against the same bare repo could transiently fail on lock
// contention; this proves they now serialize instead of racing.
func TestCreateWorktree_ConcurrentDistinctPathsSucceed(t *testing.T) {
	t.Parallel()
	if !hasGit() {
		t.Skip("git not available")
	}

	src := initRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := CloneBare(src, bare); err != nil {
		t.Fatalf("clone: %v", err)
	}
	branch, err := DefaultBranch(bare)
	if err != nil {
		t.Fatalf("default branch: %v", err)
	}

	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wtPath := filepath.Join(t.TempDir(), fmt.Sprintf("wt-%d", i))
			errs[i] = CreateWorktree(bare, wtPath, fmt.Sprintf("sybra/concurrent-%d", i), branch)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("CreateWorktree[%d]: %v", i, err)
		}
	}
}
