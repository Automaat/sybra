package sybra

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

// initToolLedger runs before initAgentManager during startup, when a.agents is
// still nil, so its own SetToolLedger call is a no-op. If initAgentManager does
// not re-bind, Logger.Log's nil-receiver guard drops every record silently: the
// ledger directory is created, no line is ever written, and nothing errors.
func TestInitAgentManagerBindsToolLedger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	cfg := config.DefaultConfig()
	cfg.Agent.Provider = "claude"
	a := &App{
		cfg:      cfg,
		logger:   discardLogger(),
		logDir:   t.TempDir(),
		agentSvc: &AgentService{},
	}

	a.initToolLedger()
	if a.toolLedger == nil {
		t.Fatal("initToolLedger did not open a ledger")
		panic("unreachable")
	}
	if err := a.initAgentManager(t.Context(), func(string, any) {}); err != nil {
		t.Fatalf("initAgentManager: %v", err)
	}
	if a.agentSvc.approval != nil {
		t.Cleanup(func() { _ = a.agentSvc.approval.Shutdown(context.Background()) })
	}

	if a.agents.ToolLedger() == nil {
		t.Fatal("agent manager has no tool ledger bound; every tool call is dropped silently")
		panic("unreachable")
	}
	if a.agents.ToolLedger() != a.toolLedger {
		t.Fatal("agent manager bound a different ledger than the app opened")
	}
}

// The ledger directory existing is not evidence that anything is recorded — it
// is created by initToolLedger regardless. This pins the distinction so an
// empty directory is read as a defect rather than as low traffic.
func TestToolLedgerDirAloneProvesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	cfg := config.DefaultConfig()
	a := &App{cfg: cfg, logger: discardLogger(), logDir: t.TempDir()}
	a.initToolLedger()

	dir := cfg.ToolLedgerDir()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("ledger dir not created: %v", err)
		panic("unreachable")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read ledger dir: %v", err)
		panic("unreachable")
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".ndjson" {
			t.Fatalf("unexpected ledger file %q before any tool call", e.Name())
		}
	}
}
