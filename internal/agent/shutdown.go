package agent

import (
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
