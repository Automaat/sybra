package agent

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestConfigureGracefulShutdown_NilSafe(t *testing.T) {
	t.Parallel()
	// Helper must tolerate nil so callers don't need a nil-check after
	// exec.CommandContext returned an error path and left cmd unset.
	configureGracefulShutdown(nil)
}

func TestConfigureGracefulShutdown_SetsCancelAndWaitDelay(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	configureGracefulShutdown(cmd)
	if cmd.Cancel == nil {
		t.Fatal("Cancel not set")
	}
	if cmd.WaitDelay != shutdownWaitDelay {
		t.Errorf("WaitDelay=%s want %s", cmd.WaitDelay, shutdownWaitDelay)
	}
}

func TestConfigureGracefulShutdown_CancelSendsSIGTERM(t *testing.T) {
	t.Parallel()
	// sleep ignores the default SIGKILL-on-cancel behavior the same as any
	// other well-behaved process: it exits on SIGTERM. If our Cancel sent
	// SIGKILL (the old default) the signal surfaced in ProcessState would
	// be "killed", not "terminated".
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "sleep", "30")
	configureGracefulShutdown(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Give the process a tick to actually be alive before we cancel,
	// otherwise Cancel fires before cmd.Process is set and the test
	// reduces to checking the nil-guard rather than the signal.
	time.Sleep(50 * time.Millisecond)
	cancel()

	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("expected exit error after cancel, got nil")
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", waitErr, waitErr)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("unexpected Sys() type %T", exitErr.Sys())
	}
	if !status.Signaled() {
		t.Fatal("process did not exit via signal")
	}
	if status.Signal() != syscall.SIGTERM {
		t.Errorf("process received %v, want SIGTERM", status.Signal())
	}
}
