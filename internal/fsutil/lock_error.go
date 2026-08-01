package fsutil

import (
	"context"
	"errors"
	"fmt"
)

// ErrLockTimeout marks a cross-process lock wait that exceeded its deadline.
var ErrLockTimeout = errors.New("fsutil: lock acquisition timed out")

// LockTimeoutError reports which lock path timed out and, when known, which pid
// last held it.
type LockTimeoutError struct {
	Path      string
	HolderPID int
	Cause     error
}

func (e *LockTimeoutError) Error() string {
	msg := fmt.Sprintf("%s: %s", ErrLockTimeout, e.Path)
	if e.HolderPID > 0 {
		msg += fmt.Sprintf(" held by pid %d", e.HolderPID)
	}
	if e.Cause != nil && !errors.Is(e.Cause, context.DeadlineExceeded) {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *LockTimeoutError) Unwrap() error { return e.Cause }

func (e *LockTimeoutError) Is(target error) bool {
	return target == ErrLockTimeout || errors.Is(e.Cause, target)
}
