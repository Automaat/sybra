//go:build !darwin && !linux

package gitexec

func processExists(_ int) bool { return false }
