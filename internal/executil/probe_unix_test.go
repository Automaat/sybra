//go:build unix

package executil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	probeTestTimeout = time.Second
	probeReturnLimit = 5 * time.Second
	descendantSleep  = "/bin/sleep 8"
)

func TestRunProbe(t *testing.T) {
	tests := []struct {
		name         string
		script       string
		wantStdout   string
		wantExitCode int
	}{
		{
			name:       "clean exit returns output",
			script:     "printf ok\n",
			wantStdout: "ok",
		},
		{
			name:       "zero exit while a descendant holds the pipes succeeds and kills it",
			script:     "printf ok\n" + descendantSleep + " &\necho $! > \"$PID_FILE\"\nexit 0\n",
			wantStdout: "ok",
		},
		{
			name:         "non-zero exit while a descendant holds the pipes keeps the exit code and kills it",
			script:       descendantSleep + " &\necho $! > \"$PID_FILE\"\nexit 7\n",
			wantExitCode: 7,
		},
		{
			name:       "zero exit kills a background helper that released the pipes",
			script:     descendantSleep + " > /dev/null 2>&1 < /dev/null &\necho $! > \"$PID_FILE\"\nprintf ok\nexit 0\n",
			wantStdout: "ok",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "descendant.pid")
			exe := writeProbeScript(t, dir, tt.script)
			ctx, cancel := context.WithTimeout(t.Context(), probeReturnLimit)
			defer cancel()

			cmd := ProbeCommand(ctx, exe)
			cmd.Env = append(os.Environ(), "PID_FILE="+pidFile)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout

			start := time.Now()
			err := RunProbe(cmd)
			elapsed := time.Since(start)

			if elapsed >= probeReturnLimit {
				t.Fatalf("RunProbe returned after %s, want under %s", elapsed, probeReturnLimit)
			}
			assertExitCode(t, err, tt.wantExitCode)
			if got := stdout.String(); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			pids := readPIDs(t, pidFile)
			if len(pids) == 0 {
				return
			}
			pid := pids[0]
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if !goneBy(start.Add(probeReturnLimit), pid) {
				t.Errorf("descendant pid %d still running after RunProbe returned", pid)
			}
		})
	}
}

func TestRunProbe_MissingBinaryReturnsError(t *testing.T) {
	cmd := ProbeCommand(t.Context(), filepath.Join(t.TempDir(), "missing"))
	if err := RunProbe(cmd); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RunProbe() = %v, want os.ErrNotExist", err)
	}
}

func TestRunProbe_DeadlineStopsSelfReinvokingWrapper(t *testing.T) {
	deadlines := []time.Duration{
		5 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		probeTestTimeout,
	}
	for _, deadline := range deadlines {
		t.Run(deadline.String(), func(t *testing.T) {
			dir := t.TempDir()
			pidsFile := filepath.Join(dir, "wrappers.pid")
			exe := writeProbeScript(t, dir, "echo $$ >> '"+pidsFile+"'\n[ \"$#\" -ge 30 ] && exec "+descendantSleep+"\n\"$0\" x \"$@\"\n")
			ctx, cancel := context.WithTimeout(t.Context(), deadline)
			defer cancel()

			start := time.Now()
			err := RunProbe(ProbeCommand(ctx, exe))
			elapsed := time.Since(start)

			pids := readPIDs(t, pidsFile)
			t.Cleanup(func() {
				for _, pid := range pids {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			if err == nil {
				t.Fatal("RunProbe() = nil, want the deadline's error")
			}
			if elapsed >= probeReturnLimit {
				t.Fatalf("RunProbe returned after %s, want under %s", elapsed, probeReturnLimit)
			}
			if deadline == probeTestTimeout && len(pids) < 2 {
				t.Fatalf("wrapper recorded %d levels, want at least 2 to exercise a chain", len(pids))
			}
			if !goneBy(start.Add(probeReturnLimit), pids...) {
				t.Errorf("wrapper chain %v still running after the deadline", pids)
			}
			if got := readPIDs(t, pidsFile); len(got) != len(pids) {
				t.Errorf("wrapper chain grew from %d to %d levels after the deadline", len(pids), len(got))
			}
		})
	}
}

func TestProbeCommand_RunsInItsOwnSession(t *testing.T) {
	cmd := ProbeCommand(t.Context(), "/bin/true")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr = %+v, want Setsid so a probe cannot stop on terminal I/O", cmd.SysProcAttr)
	}
}

func writeProbeScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}
	return path
}

func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if want == 0 {
		if err != nil {
			t.Fatalf("RunProbe() = %v, want nil", err)
		}
		return
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("RunProbe() = %v, want exit code %d", err, want)
	}
	if got := exitErr.ExitCode(); got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
}

func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	fields := strings.Fields(string(data))
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("parse pid %q: %v", field, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

func goneBy(deadline time.Time, pids ...int) bool {
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				alive = true
				break
			}
		}
		if !alive {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
