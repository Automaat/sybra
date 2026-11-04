//go:build unix

package sybra

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeProbes_OnTimeoutReturnPromptlyAndLeaveNoDescendant(t *testing.T) {
	tests := []struct {
		name  string
		probe func(path string)
	}{
		{
			name: "probeRuntimeVersion",
			probe: func(path string) {
				_, _ = probeRuntimeVersion(path, []string{"--version"}, time.Second)
			},
		},
		{
			name: "probeRuntimeTextCapability",
			probe: func(path string) {
				probe := &runtimeTextProbe{args: []string{"--help"}, substrings: []string{"--file"}}
				_ = probeRuntimeTextCapability(path, probe, time.Second)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "descendant.pid")
			exe := filepath.Join(dir, "runtime")
			writeRuntimeExe(t, exe, "#!/bin/sh\n/bin/sleep 8 &\necho $! > '"+pidFile+"'\nwait\n")

			start := time.Now()
			tt.probe(exe)
			elapsed := time.Since(start)

			pid := readRuntimeDescendantPID(t, pidFile)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if elapsed >= 5*time.Second {
				t.Fatalf("probe returned after %s, want under 5s", elapsed)
			}
			if !runtimeDescendantGoneBy(pid, start.Add(5*time.Second)) {
				t.Errorf("descendant pid %d still running 5s after the probe started", pid)
			}
		})
	}
}

func TestProbeRuntimeVersion_ReturnsVersionWhenDescendantHoldsPipes(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	exe := filepath.Join(dir, "runtime")
	writeRuntimeExe(t, exe, "#!/bin/sh\nprintf 'Runtime 1.2.3\\n'\n/bin/sleep 8 &\necho $! > '"+pidFile+"'\nexit 0\n")

	start := time.Now()
	version, probeErr := probeRuntimeVersion(exe, []string{"--version"}, 5*time.Second)
	elapsed := time.Since(start)

	pid := readRuntimeDescendantPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if version != "Runtime 1.2.3" || probeErr != "" {
		t.Errorf("probeRuntimeVersion() = (%q, %q), want (%q, \"\")", version, probeErr, "Runtime 1.2.3")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("probe returned after %s, want under 5s", elapsed)
	}
	if !runtimeDescendantGoneBy(pid, start.Add(5*time.Second)) {
		t.Errorf("descendant pid %d still running 5s after the probe started", pid)
	}
}

func readRuntimeDescendantPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse descendant pid %q: %v", data, err)
	}
	return pid
}

func runtimeDescendantGoneBy(pid int, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
