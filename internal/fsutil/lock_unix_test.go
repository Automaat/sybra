//go:build unix

package fsutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTryLockPath_SecondCallerFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sybra.lock")

	unlock, err := TryLockPath(path)
	if err != nil {
		t.Fatalf("first TryLockPath: %v", err)
		panic("unreachable")
	}
	t.Cleanup(func() {
		if err := unlock(); err != nil {
			t.Errorf("unlock: %v", err)
		}
	})

	if _, err := TryLockPath(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLockPath: want ErrLocked, got %v", err)
	}
}

func TestTryLockPath_NamesHolderPID(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sybra.lock")

	unlock, err := TryLockPath(path)
	if err != nil {
		t.Fatalf("first TryLockPath: %v", err)
		panic("unreachable")
	}
	t.Cleanup(func() {
		if err := unlock(); err != nil {
			t.Errorf("unlock: %v", err)
		}
	})

	_, err = TryLockPath(path)
	if err == nil {
		t.Fatal("expected an error from the second caller")
		panic("unreachable")
	}
	want := "held by pid " + strconv.Itoa(os.Getpid())
	if got := err.Error(); !strings.Contains(got, want) {
		t.Fatalf("error %q does not name the holder pid (want substring %q)", got, want)
	}
}

func TestTryLockPath_ReleaseAllowsReacquire(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sybra.lock")

	unlock, err := TryLockPath(path)
	if err != nil {
		t.Fatalf("first TryLockPath: %v", err)
		panic("unreachable")
	}
	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
		panic("unreachable")
	}

	unlock2, err := TryLockPath(path)
	if err != nil {
		t.Fatalf("re-acquire after unlock: %v", err)
		panic("unreachable")
	}
	if err := unlock2(); err != nil {
		t.Fatalf("unlock2: %v", err)
		panic("unreachable")
	}
}

func TestTryLockPath_CreatesParentDir(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh", "nested", "sybra.lock")

	unlock, err := TryLockPath(path)
	if err != nil {
		t.Fatalf("TryLockPath: %v", err)
		panic("unreachable")
	}
	t.Cleanup(func() {
		if err := unlock(); err != nil {
			t.Errorf("unlock: %v", err)
		}
	})

	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat parent dir: %v", err)
		panic("unreachable")
	}
}

func TestLockFileWithin_TimesOutWhenHeld(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.json")

	unlock, err := LockFile(path)
	if err != nil {
		t.Fatalf("LockFile: %v", err)
		panic("unreachable")
	}
	t.Cleanup(func() { _ = unlock() })

	start := time.Now()
	_, err = LockFileWithin(path, 30*time.Millisecond)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("LockFileWithin error = %v, want ErrLockTimeout", err)
	}
	if got := err.Error(); !strings.Contains(got, path+".lock") {
		t.Fatalf("error %q missing lock path %q", got, path+".lock")
	}
	if got := err.Error(); !strings.Contains(got, strconv.Itoa(os.Getpid())) {
		t.Fatalf("error %q missing holder pid %d", got, os.Getpid())
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("LockFileWithin took %s, want bounded wait", elapsed)
	}
}

func TestLockFileContext_CancelledWaitReturnsTypedTimeout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.json")

	unlock, err := LockFile(path)
	if err != nil {
		t.Fatalf("LockFile: %v", err)
		panic("unreachable")
	}
	defer func() { _ = unlock() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = LockFileContext(ctx, path)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("LockFileContext error = %v, want ErrLockTimeout", err)
	}
}
func TestLockFileWithin_RetriesUntilReleased(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.json")

	unlock, err := LockFile(path)
	if err != nil {
		t.Fatalf("LockFile: %v", err)
		panic("unreachable")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = unlock()
	}()

	unlock2, err := LockFileWithin(path, time.Second)
	if err != nil {
		t.Fatalf("LockFileWithin: %v", err)
		panic("unreachable")
	}
	if err := unlock2(); err != nil {
		t.Fatalf("unlock: %v", err)
		panic("unreachable")
	}
}
