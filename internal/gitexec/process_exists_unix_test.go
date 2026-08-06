//go:build darwin || linux

package gitexec

import (
	"errors"
	"syscall"
)

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
