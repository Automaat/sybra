//go:build !unix

package fsutil

import (
	"errors"
	"fmt"
)

// LockFile is unimplemented on non-unix platforms (flock has no direct
// equivalent without a platform-specific syscall). Sybra's GUI/server/CLI
// builds are unix-only; this stub exists solely so cross-platform builds of
// packages that import fsutil (e.g. `go vet ./...` on a Windows dev machine)
// still compile.
func LockFile(_ string) (func() error, error) {
	return nil, fmt.Errorf("fsutil: cross-process file locking is not supported on this platform")
}

// ErrLocked mirrors the unix build's sentinel so callers can compile
// errors.Is(err, fsutil.ErrLocked) checks on every platform, even though
// TryLockPath itself is unimplemented here.
var ErrLocked = errors.New("fsutil: already locked by another process")

// TryLockPath is unimplemented on non-unix platforms; see LockFile.
func TryLockPath(_ string) (func() error, error) {
	return nil, fmt.Errorf("fsutil: cross-process file locking is not supported on this platform")
}
