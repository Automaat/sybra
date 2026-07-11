//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func listProviderProcessesUnderRoots(_ context.Context, roots []string) []providerProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]providerProcess, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err != nil || !pathWithinRoots(cwd, roots) {
			continue
		}
		cmd := linuxProcessName(entry.Name())
		if !isProviderProcessName(cmd) {
			continue
		}
		out = append(out, providerProcess{PID: pid, Command: cmd, CWD: cwd})
	}
	return out
}

func linuxProcessName(pid string) string {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
	if err == nil && len(data) > 0 {
		if idx := bytesIndexByte(data, 0); idx >= 0 {
			data = data[:idx]
		}
		if len(data) > 0 {
			return filepath.Base(string(data))
		}
	}
	data, err = os.ReadFile(filepath.Join("/proc", pid, "comm"))
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(data)))
}

func bytesIndexByte(data []byte, target byte) int {
	for i, b := range data {
		if b == target {
			return i
		}
	}
	return -1
}
