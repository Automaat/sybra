package fsutil

import (
	"fmt"
	"log/slog"
	"sync"
)

// KeyedLocker serializes read-modify-write critical sections keyed by an
// arbitrary string (e.g. a task or project ID), both within this process (a
// ref-counted sync.Mutex per key) and across processes (an flock via
// LockFile). Sidecar stores that do a Get-then-mutate-then-write should share
// one KeyedLocker per store and hold it across the full critical section —
// not just the final write — or a concurrent writer can still interleave
// between another caller's read and write and silently drop an update.
//
// The in-process lock is acquired first (cheap, avoids paying a flock
// syscall for intra-process contention) and the cross-process flock second;
// they are released in reverse order. Entries are ref-counted so deleted or
// one-off keys do not leave lock entries behind for the lifetime of a
// long-running process.
type KeyedLocker struct {
	mu    sync.Mutex
	locks map[string]*keyRefLock
}

type keyRefLock struct {
	mu   sync.Mutex
	refs int
}

// NewKeyedLocker returns a ready-to-use KeyedLocker.
func NewKeyedLocker() *KeyedLocker {
	return &KeyedLocker{locks: map[string]*keyRefLock{}}
}

// Lock acquires the in-process mutex for key, then the cross-process flock
// on path (typically the record's own file path, or any stable synthetic
// path if the record has no single backing file), and returns a func that
// releases both in reverse order. If the flock cannot be acquired, the
// in-process lock is released before returning the error — a caller must
// never proceed holding only the in-process half, since that would silently
// reintroduce the cross-process race this lock exists to close.
func (l *KeyedLocker) Lock(key, path string) (func(), error) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*keyRefLock{}
	}
	lock := l.locks[key]
	if lock == nil {
		lock = &keyRefLock{}
		l.locks[key] = lock
	}
	lock.refs++
	l.mu.Unlock()

	lock.mu.Lock()

	releaseInProcess := func() {
		lock.mu.Unlock()
		l.mu.Lock()
		defer l.mu.Unlock()
		lock.refs--
		if lock.refs == 0 {
			delete(l.locks, key)
		}
	}

	unlockFile, err := LockFile(path)
	if err != nil {
		releaseInProcess()
		return nil, fmt.Errorf("lock %s: %w", key, err)
	}

	return func() {
		if err := unlockFile(); err != nil {
			slog.Default().Warn("fsutil.KeyedLocker.unlock_failed", "key", key, "err", err)
		}
		releaseInProcess()
	}, nil
}

// Len returns the number of keys currently holding a live lock entry. Exposed
// for tests that assert ref-counted entries are reclaimed after use.
func (l *KeyedLocker) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}
