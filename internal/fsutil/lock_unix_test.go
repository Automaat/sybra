//go:build unix

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTryLockPath_SecondCallerFails(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sybra.lock")

	unlock, err := TryLockPath(path)
	if err != nil {
		t.Fatalf("first TryLockPath: %v", err)
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
	}
	t.Cleanup(func() {
		if err := unlock(); err != nil {
			t.Errorf("unlock: %v", err)
		}
	})

	_, err = TryLockPath(path)
	if err == nil {
		t.Fatal("expected an error from the second caller")
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
	}
	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	unlock2, err := TryLockPath(path)
	if err != nil {
		t.Fatalf("re-acquire after unlock: %v", err)
	}
	if err := unlock2(); err != nil {
		t.Fatalf("unlock2: %v", err)
	}
}
