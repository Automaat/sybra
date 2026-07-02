//go:build unix

package fsutil

import (
	"fmt"
	"os"
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
