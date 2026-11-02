//go:build unix

package executil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProbeGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return killProbeGroup(cmd) }
}

// killProbeGroup returns os.ErrProcessDone once the group is empty. Darwin
// reports a group holding only zombies as EPERM rather than ESRCH.
func killProbeGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return os.ErrProcessDone
	}
	return err
}
