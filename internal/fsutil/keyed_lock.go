package fsutil

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// KeyedLocker serializes read-modify-write critical sections keyed by an
// arbitrary string (e.g. a task or project ID). LockLocal/TryLockLocal provide
// ref-counted in-process serialization; Lock/LockWithin add a cross-process
// flock via LockFile. Sidecar stores that do a Get-then-mutate-then-write
// should share one KeyedLocker per store and hold it across the full critical section —
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
	token chan struct{}
	refs  int
}

// NewKeyedLocker returns a ready-to-use KeyedLocker.
func NewKeyedLocker() *KeyedLocker {
	return &KeyedLocker{locks: map[string]*keyRefLock{}}
}

// LockLocal acquires the in-process lock for key and returns its release
// function. The key remains retained while a caller holds or waits for the
// lock and is removed after the last release.
func (l *KeyedLocker) LockLocal(key string) func() {
	lock := l.retain(key)
	lock.acquire()
	return l.localUnlockFunc(key, lock)
}

// TryLockLocal attempts to acquire key without blocking. A failed probe does
// not leave a retained entry behind. This is the non-blocking counterpart to
// LockLocal for callers that distinguish a busy key from an idle one.
func (l *KeyedLocker) TryLockLocal(key string) (func(), bool) {
	lock := l.retain(key)
	if !lock.tryAcquire() {
		l.releaseRef(key, lock)
		return nil, false
	}
	return l.localUnlockFunc(key, lock), true
}

// Lock acquires the in-process lock for key, then the cross-process flock
// on path (typically the record's own file path, or any stable synthetic
// path if the record has no single backing file), and returns a func that
// releases both in reverse order. If the flock cannot be acquired, the
// in-process lock is released before returning the error — a caller must
// never proceed holding only the in-process half, since that would silently
// reintroduce the cross-process race this lock exists to close.
func (l *KeyedLocker) Lock(key, path string) (func(), error) {
	releaseInProcess := l.LockLocal(key)

	unlockFile, err := LockFile(path)
	if err != nil {
		releaseInProcess()
		return nil, fmt.Errorf("lock %s: %w", key, err)
	}

	return l.unlockFunc(key, unlockFile, releaseInProcess), nil
}

func (l *KeyedLocker) localUnlockFunc(key string, lock *keyRefLock) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			lock.release()
			l.releaseRef(key, lock)
		})
	}
}

// LockWithin is Lock with a bounded wait across both in-process and
// cross-process lock acquisition.
func (l *KeyedLocker) LockWithin(key, path string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	lock := l.retain(key)
	if !lock.acquireUntil(deadline) {
		l.releaseRef(key, lock)
		return nil, fmt.Errorf("lock %s: %w", key, &LockTimeoutError{Path: path + ".lock"})
	}

	releaseInProcess := func() {
		lock.release()
		l.releaseRef(key, lock)
	}

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	unlockFile, err := LockFileContext(ctx, path)
	cancel()
	if err != nil {
		releaseInProcess()
		return nil, fmt.Errorf("lock %s: %w", key, err)
	}

	return l.unlockFunc(key, unlockFile, releaseInProcess), nil
}

func (l *KeyedLocker) retain(key string) *keyRefLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks = map[string]*keyRefLock{}
	}
	lock := l.locks[key]
	if lock == nil {
		lock = newKeyRefLock()
		l.locks[key] = lock
	}
	lock.refs++
	return lock
}

func (l *KeyedLocker) releaseRef(key string, lock *keyRefLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lock.refs--
	if lock.refs == 0 && l.locks[key] == lock {
		delete(l.locks, key)
	}
}

func (l *KeyedLocker) unlockFunc(key string, unlockFile func() error, releaseInProcess func()) func() {
	return func() {
		if err := unlockFile(); err != nil {
			slog.Default().Warn("fsutil.KeyedLocker.unlock_failed", "key", key, "err", err)
		}
		releaseInProcess()
	}
}

func newKeyRefLock() *keyRefLock {
	token := make(chan struct{}, 1)
	token <- struct{}{}
	return &keyRefLock{token: token}
}

func (l *keyRefLock) acquire() {
	<-l.token
}

func (l *keyRefLock) tryAcquire() bool {
	select {
	case <-l.token:
		return true
	default:
		return false
	}
}

func (l *keyRefLock) acquireUntil(deadline time.Time) bool {
	select {
	case <-l.token:
		return true
	default:
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-l.token:
			timer.Stop()
			return true
		case <-timer.C:
			return false
		}
	}
}

func (l *keyRefLock) release() {
	l.token <- struct{}{}
}

// Len returns the number of keys currently holding a live lock entry. Exposed
// for tests that assert ref-counted entries are reclaimed after use.
func (l *KeyedLocker) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}
