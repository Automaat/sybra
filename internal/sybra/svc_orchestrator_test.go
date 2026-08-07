//go:build e2e

package sybra

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
)

func newOrchSvcForTest(t *testing.T, cfgs ...agent.ManagerConfig) (*OrchestratorService, *agent.Manager) {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.DiscardHandler)
	emitted := make(chan struct{}, 16)
	emit := func(string, any) { emitted <- struct{}{} }
	cfg := agent.ManagerConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	// The orchestrator brain is the one system role a human steers via
	// SendMessage (Role.SupportsHeadlessSteer), which only takes effect when
	// the manager's own steerable default is also on.
	cfg.Runtime.HeadlessSteerable = true
	mgr := newTestAgentManager(t, ctx, emit, logger, t.TempDir(), cfg)
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
		panic("unreachable")
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
		panic("unreachable")
	}
	if a.Name != orchestratorAgentName {
		t.Errorf("agent name = %q, want %q", a.Name, orchestratorAgentName)
	}
	if a.Mode != "headless" {
		t.Errorf("agent mode = %q, want headless", a.Mode)
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
		panic("unreachable")
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

	// Shorten the wedge grace window for this test so it doesn't have to
	// sleep the real 20s production value to exercise the post-grace path.
	origGrace := orchestratorWedgeGrace
	orchestratorWedgeGrace = 50 * time.Millisecond
	t.Cleanup(func() { orchestratorWedgeGrace = origGrace })

	// Seed the exact production wedge: a steerable headless run started with
	// no kickoff prompt comes up but never takes a turn, and the
	// block_silent fake emits nothing so it never gets a session id or a
	// single stream event either. It stays StateRunning forever (see
	// orchestratorReplaceable) rather than parking in an idle state the way
	// the old conversational runner's StatePaused did. Pin the scenario via
	// ExtraEnv (appended to the subprocess env, overriding the
	// process-global FAKE_CLAUDE_SCENARIO) so a sibling e2e test cannot race
	// the seed's async subprocess exec and hand it a session-emitting scenario.
	wedged, err := mgr.Run(agent.RunConfig{
		TaskID: "wedged", Name: "wedged", Mode: "headless", Dir: t.TempDir(),
		ExtraEnv: []string{"FAKE_CLAUDE_SCENARIO=block_silent"},
	})
	if err != nil {
		t.Fatalf("seed wedged agent: %v", err)
		panic("unreachable")
	}
	// The seed agent's process comes up asynchronously, so poll rather than
	// assume. The ceiling is generous for loaded CI; the loop exits as soon
	// as the state settles.
	var lastState agent.State
	var lastSession string
	deadline := time.Now().Add(30 * time.Second)
	for {
		if a, gerr := mgr.GetAgent(wedged.ID); gerr == nil {
			lastState, lastSession = a.GetState(), a.GetSessionID()
			if lastState == agent.StateRunning && lastSession == "" && a.OutputLen() == 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("wedged agent never settled into running-no-session (last state=%q session=%q)", lastState, lastSession)
		}
		time.Sleep(20 * time.Millisecond)
	}
	svc.agentID = wedged.ID

	// orchestratorReplaceable only treats running-no-session-no-output as
	// replaceable past orchestratorWedgeGrace (strict '>'), to avoid churning
	// a healthy agent still mid-handshake — wait it out plus a small epsilon
	// so clock granularity can't leave time.Since(startedAt) exactly equal to
	// the (now-shortened) grace window.
	time.Sleep(orchestratorWedgeGrace + 50*time.Millisecond)

	// StartOrchestrator must reap the wedged brain and swap in a fresh one.
	if err := svc.StartOrchestrator(); err != nil {
		t.Fatalf("StartOrchestrator over a wedged brain should succeed: %v", err)
		panic("unreachable")
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

	svc, mgr := newOrchSvcForTest(t, agent.ManagerConfig{Runtime: agent.ManagerRuntimeConfig{MaxConcurrent: 1}})
	t.Cleanup(func() { _ = svc.StopOrchestrator() })

	// Fill the single slot with a normal agent.
	blocker, err := mgr.Run(agent.RunConfig{
		TaskID: "blocker",
		Name:   "blocker",
		Mode:   "headless",
		Prompt: "hi",
		Dir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("start blocker: %v", err)
		panic("unreachable")
	}
	t.Cleanup(func() { _ = mgr.StopAgent(blocker.ID) })

	// Orchestrator must still start despite the saturated limit.
	if err := svc.StartOrchestrator(); err != nil {
		t.Fatalf("StartOrchestrator under saturated limit: %v", err)
		panic("unreachable")
	}
}
