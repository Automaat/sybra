//go:build unix

package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/providerid"
)

func TestProbes_OnTimeoutReturnPromptlyAndLeaveNoDescendant(t *testing.T) {
	tests := []struct {
		name   string
		binary string
		probe  func(context.Context)
	}{
		{name: "ProbeClaude", binary: providerid.Claude, probe: func(ctx context.Context) { _, _ = ProbeClaude(ctx) }},
		{name: "ProbeCodex", binary: providerid.Codex, probe: func(ctx context.Context) { _, _ = ProbeCodex(ctx) }},
		{name: "ProbeCopilot", binary: providerid.Copilot, probe: func(ctx context.Context) { _, _ = ProbeCopilot(ctx) }},
		{name: "ProbeOpenCode", binary: providerid.OpenCode, probe: func(ctx context.Context) { _, _ = ProbeOpenCode(ctx) }},
		{name: "probeCodexVersion", binary: providerid.Codex, probe: func(ctx context.Context) { _ = probeCodexVersion(ctx) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "descendant.pid")
			script := "#!/bin/sh\n/bin/sleep 8 &\necho $! > '" + pidFile + "'\nwait\n"
			if err := os.WriteFile(filepath.Join(dir, tt.binary), []byte(script), 0o755); err != nil {
				t.Fatalf("write fake %s: %v", tt.binary, err)
			}
			t.Setenv("PATH", dir)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			start := time.Now()
			tt.probe(ctx)
			elapsed := time.Since(start)

			pid := readDescendantPID(t, pidFile)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			if elapsed >= 5*time.Second {
				t.Fatalf("probe returned after %s, want under 5s", elapsed)
			}
			if !descendantGoneBy(pid, start.Add(5*time.Second)) {
				t.Errorf("descendant pid %d still running 5s after the probe started", pid)
			}
		})
	}
}

func TestProbeClaude_HealthyWhenDescendantHoldsPipes(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	script := "#!/bin/sh\nprintf '{\"loggedIn\":true}'\n/bin/sleep 8 &\necho $! > '" + pidFile + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, providerid.Claude), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake CLI: %v", err)
	}
	t.Setenv("PATH", dir)

	start := time.Now()
	st, err := ProbeClaude(t.Context())
	elapsed := time.Since(start)

	pid := readDescendantPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err != nil {
		t.Fatalf("ProbeClaude() error = %v", err)
	}
	if !st.Healthy {
		t.Errorf("Healthy = false (reason %q), want true", st.Reason)
	}
	if elapsed >= 5*time.Second {
		t.Errorf("probe returned after %s, want under 5s", elapsed)
	}
	if !descendantGoneBy(pid, start.Add(5*time.Second)) {
		t.Errorf("descendant pid %d still running 5s after the probe started", pid)
	}
}

func readDescendantPID(t *testing.T, path string) int {
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

func descendantGoneBy(pid int, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
