package agent

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// shutdownWaitDelay is the grace window granted to a subprocess between
// SIGTERM and SIGKILL on context cancel. claude/codex need a few seconds
// to flush the terminal `result` NDJSON line and close their session
// logs; anything shorter shows up in ops logs as `signal: killed` with a
// truncated run log.
const shutdownWaitDelay = 15 * time.Second

// stopSIGINTGrace is the grace window between SIGINT and SIGKILL when the
// user calls StopAgent. CC v2.1.132+ uses SIGINT for graceful shutdown:
// it restores terminal modes and persists the session ID for --resume
// before exiting. 3 seconds is enough for cleanup in the common case;
// SIGKILL handles any process that ignores SIGINT.
const stopSIGINTGrace = 3 * time.Second

// stopWithSIGINT sends SIGINT to cmd's process (CC v2.1.132+ graceful shutdown)
// and escalates to SIGKILL if the process does not exit within grace. done is
// the agent's done channel — closed when the runner goroutine fully exits. A
// nil done falls back to a bare timer. Call before cancel() so SIGINT arrives
// before any context-driven SIGTERM.
func stopWithSIGINT(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	go func() {
		if done != nil {
			select {
			case <-done:
				return
			case <-time.After(grace):
			}
		} else {
			time.Sleep(grace)
		}
		_ = cmd.Process.Signal(syscall.SIGKILL)
	}()
}

// configureGracefulShutdown wires cmd so that a cancelled context first
// sends SIGTERM (letting the subprocess flush its final output) and only
// SIGKILLs after shutdownWaitDelay if it refuses to exit. The default for
// exec.CommandContext is SIGKILL-on-cancel with no grace, which is the
// source of the truncated-NDJSON "signal: killed" pattern in server logs.
//
// Safe to call on any *exec.Cmd built with exec.CommandContext before
// cmd.Start.
func configureGracefulShutdown(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = shutdownWaitDelay
}
