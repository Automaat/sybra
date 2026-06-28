package agent

import (
	"context"
	"log/slog"
	"testing"
)

func mustNewManager(tb testing.TB, ctx context.Context, emit EmitFunc, logger *slog.Logger, logDir string, cfgs ...ManagerConfig) *Manager {
	tb.Helper()
	cfg := ManagerConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	m, err := NewManager(ctx, emit, logger, logDir, cfg)
	if err != nil {
		tb.Fatalf("NewManager: %v", err)
	}
	return m
}
