package agent

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func mustNewManager(tb testing.TB, ctx context.Context, emit EmitFunc, logger *slog.Logger, logDir string, cfgs ...ManagerConfig) *Manager {
	tb.Helper()
	cfg := ManagerConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	if cfg.SandboxHome == nil {
		cfg.SandboxHome = testSandboxHome(tb)
	}
	m, err := NewManager(ctx, emit, logger, logDir, cfg)
	if err != nil {
		tb.Fatalf("NewManager: %v", err)
	}
	return m
}

// testSandboxHome returns a SandboxHome resolver satisfying every task-scoped
// Run/StartAgent call in tests with a real, existing directory, so tests that
// don't care about sandbox-home behavior aren't forced to wire one up
// individually. Tests exercising the resolver itself (missing/erroring/empty/
// non-directory) pass an explicit ManagerConfig.SandboxHome that overrides this.
func testSandboxHome(tb testing.TB) func(taskID string) (string, error) {
	dir := tb.TempDir()
	return func(string) (string, error) { return dir, nil }
}

// pollUntil polls cond every interval until it returns true or timeout
// elapses, returning whether cond was observed true. Use in place of a fixed
// time.Sleep before an action that depends on background state becoming
// ready — it succeeds as soon as the condition holds instead of waiting out
// a worst-case guess, and still bounds the wait on a slow/failing run.
func pollUntil(timeout, interval time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}
