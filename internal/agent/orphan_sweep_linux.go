//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func listProviderProcessesUnderRoots(ctx context.Context, roots []string) []providerProcess {
	if ctx == nil {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]providerProcess, 0)
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return out
		default:
		}
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
		cmd := linuxProcessName(ctx, entry.Name())
		owner := linuxProcessOwner(ctx, entry.Name())
		if owner.AgentID == "" && !isProviderProcessName(cmd) && normalizeObservedProcessPath(cwd) == cwd {
			continue
		}
		out = append(out, providerProcess{PID: pid, Command: cmd, CWD: cwd, Owner: owner})
	}
	return out
}

func linuxProcessName(ctx context.Context, pid string) string {
	if ctx == nil || ctx.Err() != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
	if err == nil && len(data) > 0 {
		if idx := bytesIndexByte(data, 0); idx >= 0 {
			data = data[:idx]
		}
		if len(data) > 0 {
			return filepath.Base(string(data))
		}
	}
	if ctx.Err() != nil {
		return ""
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

func linuxProcessOwner(ctx context.Context, pid string) processOwner {
	if ctx == nil || ctx.Err() != nil {
		return processOwner{}
	}
	data, err := os.ReadFile(filepath.Join("/proc", pid, "environ"))
	if err != nil || len(data) == 0 {
		return processOwner{}
	}
	return processOwnerFromAnyEnv(strings.Split(string(data), "\x00"))
}
