//go:build unix

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

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
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() error {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		if closeErr := f.Close(); unlockErr == nil {
			unlockErr = closeErr
		}
		return unlockErr
	}, nil
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
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write lock file: %w", err)
	}
	return func() error {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		if closeErr := f.Close(); unlockErr == nil {
			unlockErr = closeErr
		}
		return unlockErr
	}, nil
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
