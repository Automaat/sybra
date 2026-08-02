//go:build unix

package fsutil

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	lockHelperEnv     = "SYBRA_FSUTIL_LOCK_HELPER"
	lockHelperPathEnv = "SYBRA_FSUTIL_LOCK_PATH"
)

func TestLockFileHelperProcess(t *testing.T) {
	if os.Getenv(lockHelperEnv) == "" {
		return
	}
	path := os.Getenv(lockHelperPathEnv)
	unlock, err := LockFileContext(context.Background(), path)
	if err != nil {
		t.Fatalf("helper LockFileContext: %v", err)
	}
	fmt.Println("ready")
	_, _ = io.ReadAll(os.Stdin)
	if err := unlock(); err != nil {
		t.Fatalf("helper unlock: %v", err)
	}
}

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

func TestTryLockPath_CreatesParentDir(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh", "nested", "sybra.lock")

	unlock, err := TryLockPath(path)
	if err != nil {
		t.Fatalf("TryLockPath: %v", err)
	}
	t.Cleanup(func() {
		if err := unlock(); err != nil {
			t.Errorf("unlock: %v", err)
		}
	})

	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
}

func TestLockFile_TimesOutAndNamesHolderPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	restore := setLockTimingForTest(t, 150*time.Millisecond, 10*time.Millisecond)
	defer restore()

	cmd, release := startLockFileHolder(t, path)
	defer release()

	start := time.Now()
	_, err := LockFile(path)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("LockFile succeeded, want timeout")
	}
	var timeoutErr *LockTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %T %v, want *LockTimeoutError", err, err)
	}
	if timeoutErr.Path != path+".lock" {
		t.Fatalf("timeout path = %q, want %q", timeoutErr.Path, path+".lock")
	}
	if timeoutErr.HolderPID != cmd.Process.Pid {
		t.Fatalf("holder pid = %d, want %d", timeoutErr.HolderPID, cmd.Process.Pid)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("LockFile elapsed = %v, want bounded wait well under 500ms", elapsed)
	}
	if got := timeoutErr.Error(); !strings.Contains(got, path+".lock") || !strings.Contains(got, strconv.Itoa(cmd.Process.Pid)) {
		t.Fatalf("error %q missing lock path or holder pid", got)
	}
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("errors.Is(err, ErrLockTimeout) = false for %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false for %v", err)
	}
}

func TestLockFileContext_CanceledBeforeAcquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := LockFileContext(ctx, path)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func setLockTimingForTest(t *testing.T, timeout, backoff time.Duration) func() {
	t.Helper()
	prevTimeout, prevBackoff := LockAcquireTimeout, LockAcquireRetryBackoff
	LockAcquireTimeout, LockAcquireRetryBackoff = timeout, backoff
	return func() {
		LockAcquireTimeout, LockAcquireRetryBackoff = prevTimeout, prevBackoff
	}
}

func startLockFileHolder(t *testing.T, path string) (cmd *exec.Cmd, release func()) {
	t.Helper()
	var err error
	var stdout, stderr io.ReadCloser
	var stdin io.WriteCloser
	cmd = exec.Command(os.Args[0], "-test.run=^TestLockFileHelperProcess$")
	cmd.Env = append(os.Environ(),
		lockHelperEnv+"=1",
		lockHelperPathEnv+"="+path,
	)
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stderr, err = cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	stdin, err = cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	ready := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr != nil {
			ready <- readErr
			return
		}
		if strings.TrimSpace(line) != "ready" {
			ready <- fmt.Errorf("unexpected helper readiness line %q", line)
			return
		}
		ready <- nil
	}()
	select {
	case err := <-ready:
		if err != nil {
			data, _ := io.ReadAll(stderr)
			t.Fatalf("helper readiness: %v\nstderr:\n%s", err, string(data))
		}
	case <-time.After(2 * time.Second):
		data, _ := io.ReadAll(stderr)
		t.Fatalf("helper readiness timed out\nstderr:\n%s", string(data))
	}
	return cmd, func() {
		_ = stdin.Close()
		waitErr := cmd.Wait()
		if waitErr != nil {
			data, _ := io.ReadAll(stderr)
			t.Fatalf("helper wait: %v\nstderr:\n%s", waitErr, string(data))
		}
	}
}
