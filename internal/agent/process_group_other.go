//go:build !linux

package agent

import "syscall"

func processGroupActive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}
