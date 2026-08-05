package fsutil

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestKeyedLockerLockWithin_TimesOutOnInProcessContention(t *testing.T) {
	t.Parallel()

	locker := NewKeyedLocker()
	dir := t.TempDir()
	unlock, err := locker.Lock("task-a", filepath.Join(dir, "one.json"))
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	var unlockOnce sync.Once
	release := func() {
		unlockOnce.Do(unlock)
	}
	defer release()

	timer := time.AfterFunc(200*time.Millisecond, release)
	defer timer.Stop()

	start := time.Now()
	_, err = locker.LockWithin("task-a", filepath.Join(dir, "two.json"), 30*time.Millisecond)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("LockWithin error = %v, want ErrLockTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("LockWithin took %s, want bounded in-process wait", elapsed)
	}

	release()
	if got := locker.Len(); got != 0 {
		t.Fatalf("locker.Len() = %d, want timeout and release to reclaim key", got)
	}
}
