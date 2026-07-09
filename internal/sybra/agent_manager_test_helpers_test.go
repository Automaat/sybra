package sybra

import (
	"context"
	"log/slog"
	"testing"
	"time"

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
	var approvalServer *agent.ApprovalServer
	if cfg.ApprovalAddr == "" {
		srv, err := agent.NewApprovalServer(ctx, emit, logger, 0)
		if err != nil {
			tb.Fatalf("NewApprovalServer: %v", err)
		}
		approvalServer = srv
		cfg.ApprovalAddr = srv.Addr()
	}
	m, err := agent.NewManager(ctx, emit, logger, logDir, cfg)
	if err != nil {
		if approvalServer != nil {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = approvalServer.Shutdown(shutdownCtx)
			cancel()
		}
		tb.Fatalf("NewManager: %v", err)
	}
	if approvalServer != nil {
		approvalServer.SetManager(m)
		tb.Cleanup(func() {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = approvalServer.Shutdown(shutdownCtx)
			cancel()
		})
	}
	return m
}
