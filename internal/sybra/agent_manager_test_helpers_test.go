package sybra

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
)

func newTestAgentManager(tb testing.TB, ctx context.Context, emit agent.EmitFunc, logger *slog.Logger, logDir string, cfgs ...agent.ManagerConfig) *agent.Manager {
	tb.Helper()
	cfg := agent.ManagerConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	if cfg.SandboxHome == nil {
		dir := tb.TempDir()
		cfg.SandboxHome = func(string) (string, error) { return dir, nil }
	}
	m, err := agent.NewManager(ctx, emit, logger, logDir, cfg)
	if err != nil {
		tb.Fatalf("NewManager: %v", err)
	}
	return m
}
