package sybra

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
)

// TestReconcileOrchestratorsLocked_StableAfterTerminalTransition guards
// #2725: before the orchestrator singleton selection was made to ignore
// terminal registry entries, every reconciliation tick over an unchanged
// stopped orchestrator agent re-selected it via orchestratorReplaceable,
// re-called agents.StopAgent, and re-logged "orchestrator.reconciled" for as
// long as the dead agent sat in the manager's retention window. A terminal
// transition must produce its lifecycle log/side-effects exactly once; many
// subsequent reconciliation ticks over the same unchanged state must add
// none.
func TestReconcileOrchestratorsLocked_StableAfterTerminalTransition(t *testing.T) {
	ctx := t.Context()
	var records []slog.Record
	handler := &recordHandler{records: &records}
	logger := slog.New(handler)
	emit := func(string, any) {}
	mgr := newTestAgentManager(t, ctx, emit, logger, t.TempDir())

	a, err := mgr.Run(agent.RunConfig{
		TaskID: "orch-task",
		Name:   orchestratorAgentName,
		Mode:   "headless",
		Prompt: "test",
		Dir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
		panic("unreachable")
	}

	// KillAgentsForTask stops the agent and blocks until its runner goroutine
	// confirms exit, so its own logging has settled before the "before"
	// snapshot below — an in-flight straggler log line could otherwise land
	// in either window and flake the comparison.
	if !mgr.KillAgentsForTask(a.TaskID, 10*time.Second) {
		t.Fatal("agent did not exit within 10s of KillAgentsForTask")
	}

	svc := &OrchestratorService{agents: mgr, logger: logger, emit: func(string, any) {}, agentID: a.ID}

	before := len(records)
	var lastKeep string
	for range 20 {
		lastKeep = svc.reconcileOrchestratorsLocked()
	}
	after := len(records)

	if lastKeep != "" {
		t.Errorf("reconcileOrchestratorsLocked() kept = %q, want empty (terminal agent must never be kept)", lastKeep)
	}
	if after != before {
		t.Errorf("reconciliation over an unchanged terminal orchestrator kept producing log output: %d records before 20 ticks, %d after", before, after)
	}
}
