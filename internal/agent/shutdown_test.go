package agent

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// waitStatus extracts syscall.WaitStatus from an *exec.ExitError or fails the test.
func waitStatus(t *testing.T, err error) syscall.WaitStatus {
	t.Helper()
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("unexpected Sys() type %T", exitErr.Sys())
	}
	return ws
}

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
	exitErr, ok := errors.AsType[*exec.ExitError](waitErr)
	if !ok {
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

func TestStopWithSIGINT_NilSafe(t *testing.T) {
	t.Parallel()
	stopWithSIGINT(nil, nil, time.Second)
}

// TestStopWithSIGINT_ProcessReceivesInterrupt verifies that stopWithSIGINT
// sends SIGINT as the primary stop signal. A process that responds to SIGINT
// exits via that signal before the SIGKILL escalation fires, proving SIGINT
// was sent and effective. This is the normal CC graceful-shutdown path.
func TestStopWithSIGINT_ProcessReceivesInterrupt(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Wait()
	}()

	time.Sleep(20 * time.Millisecond) // let process settle
	stopWithSIGINT(cmd, done, time.Second)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit within 3s after SIGINT")
	}

	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatal("unexpected Sys() type")
	}
	if !ws.Signaled() {
		t.Fatal("expected signal exit")
	}
	if ws.Signal() != syscall.SIGINT {
		t.Errorf("process exited via %v, want SIGINT (should not need SIGKILL)", ws.Signal())
	}
}

// TestStopWithSIGINT_EscalatesToSIGKILL verifies that when a process ignores
// SIGINT and the done channel is never closed within grace, stopWithSIGINT
// escalates to SIGKILL. Asserts SIGKILL (not hang) as the exit signal.
func TestStopWithSIGINT_EscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	// bash with SIGINT ignored; sleep subprocess inherits the ignore disposition.
	cmd := exec.Command("bash", "-c", "trap '' INT; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	time.Sleep(20 * time.Millisecond) // let process settle
	// Pass a done channel that is never closed — simulates SIGINT being ignored.
	neverDone := make(chan struct{})
	stopWithSIGINT(cmd, neverDone, 150*time.Millisecond)

	select {
	case err := <-waitErr:
		ws := waitStatus(t, err)
		if !ws.Signaled() {
			t.Fatal("expected signal exit")
		}
		if ws.Signal() != syscall.SIGKILL {
			t.Errorf("got signal %v, want SIGKILL (SIGINT was ignored, escalation must fire)", ws.Signal())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process not killed within expected time after SIGKILL escalation")
	}
}
