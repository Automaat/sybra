//go:build unix

package fsutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	// LockAcquireTimeout bounds how long LockFile waits for a contended lock
	// before returning ErrLockTimeout. Tests shorten this to keep contention
	// probes cheap.
	LockAcquireTimeout = 2 * time.Second
	// LockAcquireRetryBackoff is the poll interval between LOCK_NB attempts.
	LockAcquireRetryBackoff = 25 * time.Millisecond
)

// LockFile acquires an advisory, cross-process exclusive lock for path by
// flocking a sibling "<path>.lock" file, retrying a non-blocking lock until a
// bounded deadline expires. Sybra runs the GUI server, sybra-cli, and the
// recovery sweep as separate OS processes that all read-modify-write the same
// store files; an in-process sync.Mutex only serializes goroutines within one
// of those processes; flock is what serializes across all of them.
//
// The returned unlock releases the flock and closes the file descriptor.
// Callers must call it exactly once, typically via defer, and must hold it
// across the full read-modify-write critical section (not just the final
// write) or a concurrent writer can still interleave between another
// process's read and write.
func LockFile(path string) (func() error, error) {
	ctx, cancel := context.WithTimeout(context.Background(), LockAcquireTimeout)
	unlock, err := LockFileContext(ctx, path)
	if err != nil {
		cancel()
		return nil, err
	}
	return func() error {
		defer cancel()
		return unlock()
	}, nil
}

// LockFileContext is LockFile with a caller-supplied cancellation/deadline.
func LockFileContext(ctx context.Context, path string) (func() error, error) {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	unlock, err := acquireLock(ctx, f, lockPath, true)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return unlock, nil
}

// ErrLocked is returned by TryLockPath when another process already holds
// the lock. Callers can use errors.Is to distinguish "already running" from
// other open/flock failures (permissions, missing directory, ...).
var ErrLocked = errors.New("fsutil: already locked by another process")

// TryLockPath acquires an advisory, cross-process exclusive lock on the
// exact file at path (unlike LockFile, no ".lock" suffix is appended), failing
// immediately instead of blocking if another process already holds it. This
// is the primitive behind Sybra's single-instance-per-home guard: two
// processes (desktop app, sybra-server, or a test-runner's app-under-test)
// pointed at the same SYBRA_HOME must never run concurrently, since they
// would fight over task files, in-memory agent state, and pollers. A second
// launch should fail fast with a clear "already running" error, not hang
// waiting for the first to exit.
//
// On success, the caller's pid is written into the lock file so a rejected
// second launch can name the holder. The returned unlock releases the flock,
// closes the file descriptor, and must be called exactly once.
func TryLockPath(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	unlock, err := acquireLock(context.Background(), f, path, false)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return unlock, nil
}

func acquireLock(ctx context.Context, f *os.File, path string, retry bool) (func() error, error) {
	for {
		if retry {
			if err := ctx.Err(); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return nil, &LockTimeoutError{Path: path, Cause: err}
				}
				return nil, fmt.Errorf("lock %s: %w", path, err)
			}
		}
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := writeLockHolderPID(f); err != nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				return nil, err
			}
			return func() error {
				unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				if closeErr := f.Close(); unlockErr == nil {
					unlockErr = closeErr
				}
				return unlockErr
			}, nil
		}
		holder := readLockHolderPID(f)
		if !wouldBlock(err) {
			return nil, fmt.Errorf("flock: %w", err)
		}
		if !retry {
			return nil, lockedError(holder)
		}
		if waitErr := waitForLockRetry(ctx, path, holder); waitErr != nil {
			return nil, waitErr
		}
	}
}

func waitForLockRetry(ctx context.Context, path string, holderPID int) error {
	wait := LockAcquireRetryBackoff
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &LockTimeoutError{Path: path, HolderPID: holderPID, Cause: context.DeadlineExceeded}
		}
		if remaining < wait {
			wait = remaining
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &LockTimeoutError{Path: path, HolderPID: holderPID, Cause: ctx.Err()}
		}
		return fmt.Errorf("lock %s: %w", path, ctx.Err())
	case <-timer.C:
		return nil
	}
}

func writeLockHolderPID(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return fmt.Errorf("write lock file: %w", err)
	}
	return nil
}

func lockedError(holderPID int) error {
	if holderPID > 0 {
		return fmt.Errorf("%w: held by pid %d", ErrLocked, holderPID)
	}
	return ErrLocked
}

func wouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

// readLockHolderPID best-effort reads the pid a prior TryLockPath winner
// wrote into f, for a friendlier "held by pid N" error. Returns 0 (omit from
// the error) if the file is empty, unreadable, or predates this convention.
func readLockHolderPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
