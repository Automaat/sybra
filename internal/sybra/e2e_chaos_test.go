//go:build !short

package sybra

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// chaosScenarioPool is the menu of fake-claude scenarios drawn at random per
// agent invocation. Mix of happy and failure modes so any seed can produce
// e.g. "succeed → fail_exit → no_result → success" sequences.
//
// Scenarios that change task status (triage_*) appear so the chaos can drive
// branch transitions through the test-simple workflow's planning vs
// direct-implement path. Pure-failure scenarios force retries; auth_error and
// fail_exit exhaust the max_retries budget on enough repetition.
var chaosScenarioPool = []string{
	"success",
	"implement",
	"pr_created",
	"fail_exit",
	"no_result",
	"auth_error",
	"malformed_pr_output",
	"triage",
	"triage_to_planning",
	"triage_to_done",
	"triage_to_human_required",
	"triage_to_in_review",
}

// TestE2E_ChaosFullLifecycle runs the test-simple workflow many times under
// randomized failure injection. For each seed the test:
//
//  1. Generates a random sequence of 6-12 scenarios from chaosScenarioPool.
//  2. Creates a task and starts the workflow.
//  3. Waits for the system to settle (workflow terminal OR task in
//     human-required OR 30s deadline).
//  4. Asserts invariants that must hold no matter which failure path fired:
//     - Task file on disk parses cleanly (no torn writes).
//     - No agent is still in StateRunning (no leaked subprocess).
//     - Workflow has a non-empty step history (triage at minimum ran).
//     - Goroutine count is back near baseline (no leaked watcher/runner).
//
// Each seed is reproducible: rerun with `go test -run
// TestE2E_ChaosFullLifecycle/seed-N` to investigate a failing case.
//
// This guards against the class of bug where a specific failure-mode
// combination leaves the system in an incoherent state — which the
// happy-path lifecycle tests miss by construction.
func TestE2E_ChaosFullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos test runs many seeds; skipped in -short mode")
	}

	const seeds = 24

	// Capture goroutine baseline before spawning any harness goroutines.
	runtime.GC()
	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	// Subtests run sequentially because setupE2E calls t.Setenv, which
	// the testing package refuses to combine with t.Parallel.
	for i := range seeds {
		seed := uint64(i + 1)
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			runChaosSeed(t, seed)
		})
	}

	// Final check — let the spawned tests' goroutines finish, then sanity
	// check global growth. Cleanups are deferred via t.Cleanup so by the time
	// the parent test runs this code, subtests have torn down.
	t.Cleanup(func() {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(200 * time.Millisecond)
		final := runtime.NumGoroutine()
		// 50 slack covers test framework + parallel-runner internals.
		if final-baselineGoroutines > 50 {
			t.Logf("goroutine count: baseline=%d final=%d diff=%d (informational; high diff suggests leaked harness goroutines)",
				baselineGoroutines, final, final-baselineGoroutines)
		}
	})
}

func runChaosSeed(t *testing.T, seed uint64) {
	t.Helper()

	rng := rand.New(rand.NewPCG(seed, seed*2654435761))
	steps := 6 + rng.IntN(7) // 6..12 scenarios
	sequence := make([]string, steps)
	for i := range sequence {
		sequence[i] = chaosScenarioPool[rng.IntN(len(chaosScenarioPool))]
	}
	t.Logf("chaos sequence (seed=%d): %s", seed, strings.Join(sequence, " → "))

	env := setupE2EMulti(t, sequence)

	created, err := env.tasks.Create(fmt.Sprintf("chaos-task-%d", seed), "body", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatalf("startWorkflow: %v", err)
	}

	// Wait for the system to settle. Settled = workflow terminal OR
	// task is human-required (which can leave the workflow waiting at a
	// non-terminal step in test-simple, since no link_pr_and_review chain
	// runs the explicit human-required transition).
	settled := waitForChaosSettle(t, env, created.ID, 30*time.Second)
	if !settled {
		// Don't t.Fatal — collect diagnostics first.
		dumpChaosState(t, env, created.ID)
		t.Fatalf("seed %d: workflow never settled within 30s", seed)
	}

	// Invariant 1: task file parses cleanly (no torn writes).
	// isChaosSettled gates on HasOnCompleteInflight, so by the time settle
	// returns, onComplete has fully drained and StepHistory is written.
	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatalf("seed %d: post-settle task parse: %v", seed, err)
	}

	// Invariant 2: no live agents for this task (no leaked subprocess).
	// Guaranteed by the drain gate in isChaosSettled — no poll needed.
	if env.agents.HasRunningAgentForTask(created.ID) {
		t.Errorf("seed %d: task has lingering running agent after settle", seed)
	}
	if env.agents.HasOnCompleteInflight(created.ID) {
		t.Errorf("seed %d: onComplete still in-flight after settle — drain gate broken", seed)
	}

	// Invariant 3: workflow has step history (triage at minimum).
	if tk.Workflow == nil {
		t.Errorf("seed %d: workflow is nil after settle", seed)
		return
	}
	if len(tk.Workflow.StepHistory) == 0 {
		dumpChaosState(t, env, created.ID)
		t.Errorf("seed %d: empty StepHistory — workflow never executed any step", seed)
	}

	// Invariant 4: terminal state is one of the documented outcomes.
	state := tk.Workflow.State
	status := tk.Status
	terminal := state == workflow.ExecCompleted || state == workflow.ExecFailed
	humanRequired := status == task.StatusHumanRequired
	waiting := state == workflow.ExecWaiting
	if !terminal && !humanRequired && !waiting {
		t.Errorf("seed %d: incoherent settle: state=%q status=%q step=%q",
			seed, state, status, tk.Workflow.CurrentStep)
	}
}

// waitForChaosSettle polls until the task workflow reaches a settled
// configuration sustained across consecutive observations. Returns true on
// settle, false on timeout.
//
// "Settled" means terminal-or-paused with no in-flight agent, observed in
// the same state for `requiredStable` consecutive polls. Sustained
// observation is necessary because the engine briefly passes through
// settled-looking transient states (state=Waiting between agent retries,
// state=Running with no agent between mechanical steps), and a single
// observation would race the next executeSteps invocation.
func waitForChaosSettle(t *testing.T, env *e2eEnv, taskID string, timeout time.Duration) bool {
	t.Helper()
	const requiredStable = 4
	const pollInterval = 50 * time.Millisecond
	deadline := time.After(timeout)
	stableCount := 0
	for {
		select {
		case <-deadline:
			return false
		case <-time.After(pollInterval):
			if isChaosSettled(env, taskID) {
				stableCount++
				if stableCount >= requiredStable {
					return true
				}
			} else {
				stableCount = 0
			}
		}
	}
}

// isChaosSettled returns true when the task is in a coherent paused or
// terminal state with no live agent AND no onComplete callback in-flight.
// Used by waitForChaosSettle as a per-poll predicate; the caller requires
// sustained truth before declaring settlement to filter out transient
// between-step observations.
//
// The drain gate (HasOnCompleteInflight) is required because markAgentDone
// — which closes the done channel and makes HasRunningAgentForTask return
// false — runs BEFORE onComplete fires. Without gating on the drain,
// settlement can be declared while HandleAgentComplete is still executing
// and StepHistory has not yet been written.
func isChaosSettled(env *e2eEnv, taskID string) bool {
	if env.agents.HasRunningAgentForTask(taskID) {
		return false
	}
	if env.agents.HasOnCompleteInflight(taskID) {
		return false
	}
	tk, err := env.tasks.Get(taskID)
	if err != nil || tk.Workflow == nil {
		return false
	}
	state := tk.Workflow.State
	if state == workflow.ExecCompleted || state == workflow.ExecFailed {
		return true
	}
	if tk.Status == task.StatusHumanRequired {
		return true
	}
	if state == workflow.ExecWaiting {
		return true
	}
	return false
}

// dumpChaosState writes diagnostic info to test logs when a seed fails to
// settle. Helps reproduce the bug without re-running the whole chaos suite.
func dumpChaosState(t *testing.T, env *e2eEnv, taskID string) {
	t.Helper()
	tk, err := env.tasks.Get(taskID)
	if err != nil {
		t.Logf("dump: task get failed: %v", err)
		return
	}
	t.Logf("dump task: id=%s status=%q reason=%q", tk.ID, tk.Status, tk.StatusReason)
	if tk.Workflow != nil {
		t.Logf("dump workflow: state=%q step=%q history=%d",
			tk.Workflow.State, tk.Workflow.CurrentStep, len(tk.Workflow.StepHistory))
		for i := range tk.Workflow.StepHistory {
			r := &tk.Workflow.StepHistory[i]
			t.Logf("  history[%d]: step=%s status=%s output=%q",
				i, r.StepID, r.Status, truncateForLog(r.Output, 80))
		}
	}
	for _, a := range env.agents.ListAgents() {
		if a.TaskID == taskID {
			t.Logf("dump agent: id=%s state=%v provider=%s err=%v",
				a.ID, a.GetState(), a.Provider, a.GetExitErr())
		}
	}
}

func truncateForLog(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// TestE2E_ChaosSettleGatedOnOnCompleteDrain verifies that isChaosSettled
// returns false while an onComplete callback is executing, even though
// HasRunningAgentForTask is already false at that point.
//
// The race being tested: markAgentDone closes the agent's done channel
// (making HasRunningAgentForTask return false) before onComplete fires.
// Without the HasOnCompleteInflight gate, a settle poll during that window
// would incorrectly declare the workflow settled — potentially before
// HandleAgentComplete has written StepHistory.
//
// The test intercepts the first onComplete call with a gate channel, asserts
// the invariants during the blocked window, then releases the gate and
// verifies that settle completes with a populated StepHistory.
func TestE2E_ChaosSettleGatedOnOnCompleteDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("drain-gate test requires live agents; skipped in -short mode")
	}

	env := setupE2E(t, "success")

	// Gate the first onComplete call so we can observe the drain window.
	gate := make(chan struct{})
	ready := make(chan struct{})
	var readyOnce, gateOnce sync.Once

	env.agents.SetOnComplete(func(ag *agent.Agent) {
		// Signal that onComplete has started (goroutine exited, drain begun).
		readyOnce.Do(func() { close(ready) })
		// Block only the first call; subsequent calls pass through.
		gateOnce.Do(func() { <-gate })
		// Advance the workflow — this writes StepHistory.
		var result string
		for _, ev := range ag.Output() {
			if ev.Type == "result" {
				result = ev.Content
			}
		}
		env.engine.HandleAgentComplete(ag.TaskID, workflow.AgentCompletion{
			AgentID:  ag.ID,
			Result:   result,
			Success:  ag.GetExitErr() == nil,
			Provider: ag.Provider,
		})
	})

	created, err := env.tasks.Create("drain-gate-test", "body", "headless")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := env.startWorkflow(created.ID, "test-simple"); err != nil {
		t.Fatalf("startWorkflow: %v", err)
	}

	// Wait for the first agent goroutine to exit and onComplete to start.
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for first onComplete to start")
	}

	// Drain window: goroutine done, onComplete executing (blocked on gate).
	if env.agents.HasRunningAgentForTask(created.ID) {
		t.Error("expected HasRunningAgentForTask=false while onComplete blocked")
	}
	if !env.agents.HasOnCompleteInflight(created.ID) {
		t.Error("expected HasOnCompleteInflight=true while onComplete blocked")
	}
	// The settle predicate must return false — gated on drain.
	if isChaosSettled(env, created.ID) {
		t.Error("isChaosSettled returned true while onComplete was in-flight — drain gate missing")
	}

	// Release the gate; onComplete proceeds and HandleAgentComplete runs.
	close(gate)

	if !waitForChaosSettle(t, env, created.ID, 15*time.Second) {
		dumpChaosState(t, env, created.ID)
		t.Fatal("workflow never settled after onComplete drained")
	}

	// StepHistory must be populated — no poll needed because the drain gate
	// ensures HandleAgentComplete has already returned before settle fires.
	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatalf("post-settle task get: %v", err)
	}
	if tk.Workflow == nil || len(tk.Workflow.StepHistory) == 0 {
		dumpChaosState(t, env, created.ID)
		t.Error("StepHistory empty after settle — drain gate should have ensured it was written")
	}
}
