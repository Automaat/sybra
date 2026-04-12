package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/Automaat/synapse/internal/agent"
)

func newOrchSvcForTest(t *testing.T) (*OrchestratorService, *agent.Manager, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	emitted := make(chan struct{}, 16)
	emit := func(string, any) { emitted <- struct{}{} }
	mgr := agent.NewManager(ctx, emit, logger, t.TempDir())
	svc := &OrchestratorService{
		agents: mgr,
		logger: logger,
		emit:   func(string, any) {},
	}
	return svc, mgr, cancel
}

func TestOrchestratorService_StartStopLifecycle(t *testing.T) {
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "interactive_implement")
	t.Setenv("SYNAPSE_HOME", t.TempDir())

	svc, _, cancel := newOrchSvcForTest(t)
	defer cancel()
	t.Cleanup(func() { _ = svc.StopOrchestrator() })

	if err := svc.StartOrchestrator(); err != nil {
		t.Fatalf("StartOrchestrator: %v", err)
	}

	id := svc.GetOrchestratorAgentID()
	if id == "" {
		t.Fatal("expected non-empty agent id after start")
	}

	if !svc.IsOrchestratorRunning() {
		t.Fatal("expected orchestrator running")
	}

	a, err := svc.agents.GetAgent(id)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.Name != orchestratorAgentName {
		t.Errorf("agent name = %q, want %q", a.Name, orchestratorAgentName)
	}
	if a.Mode != "interactive" {
		t.Errorf("agent mode = %q, want interactive", a.Mode)
	}
	if a.Provider != "claude" {
		t.Errorf("agent provider = %q, want claude (orchestrator must pin claude even when cfg=codex)", a.Provider)
	}

	// Starting again must fail.
	if err := svc.StartOrchestrator(); err == nil {
		t.Error("expected second StartOrchestrator to fail")
	}

	if err := svc.StopOrchestrator(); err != nil {
		t.Fatalf("StopOrchestrator: %v", err)
	}
	if svc.GetOrchestratorAgentID() != "" {
		t.Error("agent id should be empty after stop")
	}
	if svc.IsOrchestratorRunning() {
		t.Error("IsOrchestratorRunning should be false after stop")
	}
}

func TestOrchestratorService_StopWhenNotRunning(t *testing.T) {
	svc, _, cancel := newOrchSvcForTest(t)
	defer cancel()

	if err := svc.StopOrchestrator(); err == nil {
		t.Error("expected error stopping an orchestrator that was never started")
	}
}

func TestOrchestratorService_IgnoreConcurrencyLimit(t *testing.T) {
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "interactive_implement")
	t.Setenv("SYNAPSE_HOME", t.TempDir())

	svc, mgr, cancel := newOrchSvcForTest(t)
	defer cancel()
	mgr.SetMaxConcurrent(1)
	t.Cleanup(func() { _ = svc.StopOrchestrator() })

	// Fill the single slot with a normal agent.
	blocker, err := mgr.Run(agent.RunConfig{
		TaskID: "blocker",
		Name:   "blocker",
		Mode:   "interactive",
		Prompt: "hi",
		Dir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("start blocker: %v", err)
	}
	t.Cleanup(func() { _ = mgr.StopAgent(blocker.ID) })

	// Orchestrator must still start despite the saturated limit.
	if err := svc.StartOrchestrator(); err != nil {
		t.Fatalf("StartOrchestrator under saturated limit: %v", err)
	}
}
