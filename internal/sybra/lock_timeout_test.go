package sybra

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
)

const (
	taskLockHelperEnv     = "SYBRA_TASK_LOCK_HELPER"
	taskLockHelperPathEnv = "SYBRA_TASK_LOCK_PATH"
)

func TestTaskLockHelperProcess(t *testing.T) {
	if os.Getenv(taskLockHelperEnv) == "" {
		return
	}
	path := os.Getenv(taskLockHelperPathEnv)
	unlock, err := fsutil.LockFileContext(context.Background(), path)
	if err != nil {
		t.Fatalf("helper LockFileContext: %v", err)
	}
	fmt.Println("ready")
	_, _ = io.ReadAll(os.Stdin)
	if err := unlock(); err != nil {
		t.Fatalf("helper unlock: %v", err)
	}
}

func setTaskLockTimingForTest(t *testing.T, timeout, backoff time.Duration) func() {
	t.Helper()
	prevTimeout, prevBackoff := fsutil.LockAcquireTimeout, fsutil.LockAcquireRetryBackoff
	fsutil.LockAcquireTimeout, fsutil.LockAcquireRetryBackoff = timeout, backoff
	return func() {
		fsutil.LockAcquireTimeout, fsutil.LockAcquireRetryBackoff = prevTimeout, prevBackoff
	}
}

func startTaskLockHolder(t *testing.T, path string) (cmd *exec.Cmd, release func()) {
	t.Helper()
	var err error
	var stdout, stderr io.ReadCloser
	var stdin io.WriteCloser
	cmd = exec.Command(os.Args[0], "-test.run=^TestTaskLockHelperProcess$")
	cmd.Env = append(os.Environ(),
		taskLockHelperEnv+"=1",
		taskLockHelperPathEnv+"="+path,
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
