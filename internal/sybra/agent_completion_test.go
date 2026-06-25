package sybra

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestIsSignalKill(t *testing.T) {
	t.Parallel()

	// Helper: run "sh -c 'exit N'" and assert the error has the expected code.
	exitErr := func(t *testing.T, code int) error {
		t.Helper()
		err := exec.Command("sh", "-c", "exit "+itoa(code)).Run()
		if err == nil {
			t.Fatalf("expected non-nil error for exit %d", code)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("expected *exec.ExitError for exit %d, got %T: %v", code, err, err)
		}
		if ee.ExitCode() != code {
			t.Fatalf("exit code mismatch: got %d want %d", ee.ExitCode(), code)
		}
		return err
	}

	// Helper: send SIGKILL to a running process and return the Wait error.
	sigkillErr := func(t *testing.T) error {
		t.Helper()
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleep: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("signal: %v", err)
		}
		return cmd.Wait()
	}

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want bool
	}{
		{
			name: "nil",
			err:  func(*testing.T) error { return nil },
			want: false,
		},
		{
			name: "non-ExitError",
			err:  func(*testing.T) error { return errors.New("boom") },
			want: false,
		},
		{
			name: "exit 1 (genuine failure)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 1)
			},
			want: false,
		},
		{
			name: "exit 2 (normal failure, not a signal code)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 2)
			},
			want: false,
		},
		{
			name: "exit 130 (128+SIGINT)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 130)
			},
			want: true,
		},
		{
			name: "exit 143 (128+SIGTERM)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 143)
			},
			want: true,
		},
		{
			name: "exit 137 (128+SIGKILL)",
			err: func(t *testing.T) error {
				t.Helper()
				return exitErr(t, 137)
			},
			want: true,
		},
		{
			name: "truly signaled process (ws.Signaled==true)",
			err:  sigkillErr,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.err(t)
			got := isSignalKill(err)
			if got != tc.want {
				t.Errorf("isSignalKill(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}

// itoa converts a small non-negative int to its decimal string representation
// without importing strconv (avoids an extra import just for test helpers).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
