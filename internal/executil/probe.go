package executil

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

// probeWaitDelay bounds how long a probe waits for its output pipes to close
// after it exits or is cancelled. Without it, a descendant holding those pipes
// blocks Wait for as long as the descendant lives.
const probeWaitDelay = time.Second

const (
	probeKillAttempts = 20
	probeKillInterval = 10 * time.Millisecond
)

// ProbeCommand returns a command for a short-lived CLI probe, in its own session
// so it cannot stop on terminal I/O. Cancelling ctx kills the session's process
// group, so a wrapper script cannot leave children running past the probe's
// deadline. A descendant that moves to another group with setsid or setpgid
// escapes the kill. Run the command with RunProbe.
func ProbeCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = probeWaitDelay
	configureProbeGroup(cmd)
	return cmd
}

// RunProbe runs a command from ProbeCommand, then kills what is left of its
// process group, so a wrapper that exits zero cannot leave a background loop
// running. A probe that exited zero while a descendant still held its output
// pipes counts as success with the output read so far; anything written more
// than probeWaitDelay after the exit is lost.
func RunProbe(cmd *exec.Cmd) error {
	err := cmd.Run()
	reapProbeGroup(cmd)
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

// reapProbeGroup repeats the kill until the group is empty, because a kill can
// miss a child that is mid-fork, and that child can carry a wrapper loop on.
func reapProbeGroup(cmd *exec.Cmd) {
	for range probeKillAttempts {
		if killProbeGroup(cmd) != nil {
			return
		}
		time.Sleep(probeKillInterval)
	}
}
