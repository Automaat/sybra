//go:build e2e

package sybra

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
)

func newOrchSvcForTest(t *testing.T) (*OrchestratorService, *agent.Manager) {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.DiscardHandler)
	emitted := make(chan struct{}, 16)
	emit := func(string, any) { emitted <- struct{}{} }
	mgr := agent.NewManager(ctx, emit, logger, t.TempDir())
	svc := &OrchestratorService{
		agents: mgr,
		logger: logger,
		emit:   func(string, any) {},
	}
	return svc, mgr
}

func TestOrchestratorService_StartStopLifecycle(t *testing.T) {
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "interactive_implement")
	t.Setenv("SYBRA_HOME", t.TempDir())

	svc, _ := newOrchSvcForTest(t)
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
	svc, _ := newOrchSvcForTest(t)

	if err := svc.StopOrchestrator(); err == nil {
		t.Error("expected error stopping an orchestrator that was never started")
	}
}

func TestOrchestratorService_ReplacesWedgedBrain(t *testing.T) {
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "interactive_implement")
	t.Setenv("SYBRA_HOME", t.TempDir())

	svc, mgr := newOrchSvcForTest(t)
	t.Cleanup(func() { _ = svc.StopOrchestrator() })

	// Seed the exact production wedge: a conversational agent started with no
	// kickoff prompt parks in StatePaused, and the block_silent fake emits
	// nothing so it never gets a session id. Before the fix this paused-no-
	// session state was treated as "running" and never replaced. Pin the
	// scenario via ExtraEnv (appended to the subprocess env, overriding the
	// process-global FAKE_CLAUDE_SCENARIO) so a sibling e2e test cannot race
	// the seed's async subprocess exec and hand it a session-emitting scenario.
	wedged, err := mgr.Run(agent.RunConfig{
		TaskID: "wedged", Name: "wedged", Mode: "interactive", Dir: t.TempDir(),
		ExtraEnv: []string{"FAKE_CLAUDE_SCENARIO=block_silent"},
	})
	if err != nil {
		t.Fatalf("seed wedged agent: %v", err)
	}
	// The seed agent reaches StatePaused asynchronously once its runner
	// spawns the (silent) process, so poll rather than assume. The ceiling is
	// generous for loaded CI; the loop exits as soon as the state settles.
	var lastState agent.State
	var lastSession string
	deadline := time.Now().Add(30 * time.Second)
	for {
		if a, gerr := mgr.GetAgent(wedged.ID); gerr == nil {
			lastState, lastSession = a.GetState(), a.GetSessionID()
			if lastState == agent.StatePaused && lastSession == "" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("wedged agent never parked in paused-no-session (last state=%q session=%q)", lastState, lastSession)
		}
		time.Sleep(20 * time.Millisecond)
	}
	svc.agentID = wedged.ID

	// orchestratorReplaceable must treat paused-no-session as replaceable, so
	// StartOrchestrator reaps the wedged brain and swaps in a fresh one.
	if err := svc.StartOrchestrator(); err != nil {
		t.Fatalf("StartOrchestrator over a wedged brain should succeed: %v", err)
	}
	if got := svc.GetOrchestratorAgentID(); got == "" || got == wedged.ID {
		t.Errorf("expected a fresh orchestrator id, got %q (wedged was %q)", got, wedged.ID)
	}
}

func TestOrchestratorService_IgnoreConcurrencyLimit(t *testing.T) {
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "interactive_implement")
	t.Setenv("SYBRA_HOME", t.TempDir())

	svc, mgr := newOrchSvcForTest(t)
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
