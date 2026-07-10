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
var stopSIGINTGrace = 3 * time.Second

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

// configureDetached puts the subprocess in its own session (Setsid) so it
// survives both the app's exit and a terminal SIGINT (Ctrl-C of `mise run
// dev` sends the signal to the whole foreground process group; a new
// session is immune). Detached agents are spawned with exec.Command (no
// Context), so a cancelled context never reaches the child — the only
// kill paths are an explicit StopAgent or signalKill. The output stream
// is the child's log file (written directly), not a pipe that breaks when
// the parent dies. Reattachment on the next startup tails that file.
func configureDetached(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// signalPID sends SIGINT to a detached/reattached process by PID and
// escalates to SIGKILL after the grace window. Used when there is no
// *exec.Cmd handle (a reattached agent) or to force a detached child to
// stop on a guardrail kill.
func signalPID(pid int, grace time.Duration) {
	if pid <= 0 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Signal(os.Interrupt)
	go func() {
		time.Sleep(grace)
		if processAlive(pid) {
			_ = p.Signal(syscall.SIGKILL)
		}
	}()
}
