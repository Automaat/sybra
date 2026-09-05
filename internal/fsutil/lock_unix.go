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

const lockRetryInterval = 10 * time.Millisecond

// LockTimeoutError reports that a file lock could not be acquired before the
// caller's deadline. holder PID is best-effort from the lock file contents.
type LockTimeoutError struct {
	Path      string
	HolderPID int
}

func (e *LockTimeoutError) Error() string {
	if e == nil {
		return ErrLockTimeout.Error()
	}
	if e.HolderPID > 0 {
		return fmt.Sprintf("%s: %s (held by pid %d)", ErrLockTimeout, e.Path, e.HolderPID)
	}
	return fmt.Sprintf("%s: %s", ErrLockTimeout, e.Path)
}

func (e *LockTimeoutError) Is(target error) bool {
	return target == ErrLockTimeout
}

// LockFile acquires an advisory, cross-process exclusive lock for path by
// flocking a sibling "<path>.lock" file, blocking until it is held. Sybra
// runs the GUI server, sybra-cli, and the recovery sweep as separate OS
// processes that all read-modify-write the same store files; an in-process
// sync.Mutex only serializes goroutines within one of those processes; flock
// is what serializes across all of them.
//
// The returned unlock releases the flock and closes the file descriptor.
// Callers must call it exactly once, typically via defer, and must hold it
// across the full read-modify-write critical section (not just the final
// write) or a concurrent writer can still interleave between another
// process's read and write.
func LockFile(path string) (func() error, error) {
	return lockFileUntil(path, time.Time{}, nil)
}

// LockFileContext retries non-blocking flock attempts until ctx is cancelled.
// Cancellation is reported as a typed ErrLockTimeout so callers can treat it
// as a retryable busy-peer condition while still bounding the wait.
func LockFileContext(ctx context.Context, path string) (func() error, error) {
	if ctx == nil {
		return LockFile(path)
	}
	return lockFileUntil(path, time.Time{}, ctx.Done())
}

// ErrLockTimeout is returned when LockFileWithin cannot acquire the lock
// before its deadline. Callers can use errors.Is to distinguish a wedged or
// busy peer process from an open or flock failure.
var ErrLockTimeout = errors.New("fsutil: timed out waiting for file lock")

// LockFileWithin acquires LockFile's sibling lock, retrying non-blocking flock
// attempts until timeout expires. It prevents a stalled peer process from
// indefinitely blocking a hot in-process mutex while retaining the full
// read-modify-write critical section once the lock is acquired.
func LockFileWithin(path string, timeout time.Duration) (func() error, error) {
	return lockFileUntil(path, time.Now().Add(timeout), nil)
}

func lockFileUntil(path string, deadline time.Time, done <-chan struct{}) (func() error, error) {
	lockPath := path + ".lock"
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open lock file: %w", err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := writeLockHolderPID(f); err != nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
				return nil, err
			}
			return unlockFunc(f), nil
		}
		holder := readLockHolderPID(f)
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("flock: %w", err)
		}
		if timedOut(deadline, done) {
			return nil, &LockTimeoutError{Path: lockPath, HolderPID: holder}
		}
		if err := waitForLockRetry(deadline, done); err != nil {
			return nil, &LockTimeoutError{Path: lockPath, HolderPID: holder}
		}
	}
}

func timedOut(deadline time.Time, done <-chan struct{}) bool {
	if done != nil {
		select {
		case <-done:
			return true
		default:
		}
	}
	return !deadline.IsZero() && !time.Now().Before(deadline)
}

func waitForLockRetry(deadline time.Time, done <-chan struct{}) error {
	delay := lockRetryInterval
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrLockTimeout
		}
		if remaining < delay {
			delay = remaining
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	if done == nil {
		<-timer.C
		return nil
	}

	select {
	case <-timer.C:
		return nil
	case <-done:
		return ErrLockTimeout
	}
}

func unlockFunc(f *os.File) func() error {
	return func() error {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		if closeErr := f.Close(); unlockErr == nil {
			unlockErr = closeErr
		}
		return unlockErr
	}
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readLockHolderPID(f)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if holder > 0 {
				return nil, fmt.Errorf("%w: held by pid %d", ErrLocked, holder)
			}
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	if err := writeLockHolderPID(f); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	return unlockFunc(f), nil
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

// readLockHolderPID best-effort reads the pid a prior lock winner wrote into
// f, for a friendlier "held by pid N" error. Returns 0 (omit from the error)
// if the file is empty, unreadable, or predates this convention.
func readLockHolderPID(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
