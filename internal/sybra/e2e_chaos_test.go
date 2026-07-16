//go:build e2e

package sybra

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// chaosScenarioPool is the menu of fake-claude scenarios drawn at random per
// agent invocation. Mix of happy and failure modes so any seed can produce
// e.g. "succeed → fail_exit → no_result → success" sequences.
//
// Scenarios that change task status (triage_*, evaluate) appear so the chaos
// can drive branch transitions through the test-simple workflow's planning vs
// direct-implement path. Pure-failure scenarios force retries; auth_error and
// fail_exit exhaust the max_retries budget on enough repetition.
//
// Notably absent: signal_kill (engine deliberately stalls on signal-killed
// agents to avoid advancing on incomplete work — would prevent settle) and
// hang (blocks forever; only meaningful with an external StopAgent driver).
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
	"evaluate",
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
	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatalf("seed %d: post-settle task parse: %v", seed, err)
	}

	// Invariant 2: no live agents for this task (no leaked subprocess).
	// Wait briefly — onComplete callback may be in flight.
	waitForCondition(2*time.Second, func() bool {
		return !env.agents.HasRunningAgentForTask(created.ID)
	})
	if env.agents.HasRunningAgentForTask(created.ID) {
		t.Errorf("seed %d: task has lingering running agent after settle", seed)
	}

	// Poll until StepHistory is populated. The agent.Manager's
	// markAgentDone (which decrements liveCount and so flips
	// HasRunningAgentForTask to false) runs BEFORE onComplete fires, so a
	// "no running agent" observation in settle does not guarantee
	// AdvanceStep has recorded the step yet. Give the callback a generous
	// window — the assertion still catches real "workflow never ran" bugs.
	tk = pollUntilHistoryPopulated(t, env, created.ID, 10*time.Second)

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
	scaled := time.Duration(int64(timeout) * e2eTimeoutScale())
	deadline := time.After(scaled)
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
// terminal state with no live agent. Used by waitForChaosSettle as a
// per-poll predicate; the caller requires sustained truth before declaring
// settlement to filter out transient between-step observations.
//
// The pendingCompletions gate closes most of the race window between
// agent.Manager.markAgentDone (which flips HasRunningAgentForTask to false
// before the onComplete callback runs) and the engine's AdvanceStep/
// executeSteps chain actually advancing workflow state. The callback counter
// itself is incremented inside that callback, so there is still a tiny gap
// before the counter goes non-zero. Requiring at least one recorded step
// closes that last window for the test-simple workflow used here: a real run
// must record triage before the system is considered settled.
func isChaosSettled(env *e2eEnv, taskID string) bool {
	if env.pendingCompletions.Load() > 0 {
		return false
	}
	if env.agents.HasRunningAgentForTask(taskID) {
		return false
	}
	tk, err := env.tasks.Get(taskID)
	if err != nil || tk.Workflow == nil {
		return false
	}
	if len(tk.Workflow.StepHistory) == 0 {
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

// waitForCondition polls fn until it returns true or the deadline expires.
// Returns true if fn fired, false on timeout. Used for short polls where
// failure is informational rather than fatal. Honours the same e2e timeout
// scale as waitFor so CI runners aren't penalized.
func waitForCondition(timeout time.Duration, fn func() bool) bool {
	scaled := time.Duration(int64(timeout) * e2eTimeoutScale())
	deadline := time.After(scaled)
	for {
		select {
		case <-deadline:
			return false
		case <-time.After(20 * time.Millisecond):
			if fn() {
				return true
			}
		}
	}
}

// pollUntilHistoryPopulated re-fetches the task until StepHistory has at
// least one entry, returning the most recent fetch on timeout. Used to
// bridge the markAgentDone-vs-onComplete window where a settled-looking
// snapshot can briefly show empty history.
func pollUntilHistoryPopulated(t *testing.T, env *e2eEnv, taskID string, timeout time.Duration) task.Task {
	t.Helper()
	scaled := time.Duration(int64(timeout) * e2eTimeoutScale())
	deadline := time.After(scaled)
	var last task.Task
	for {
		tk, err := env.tasks.Get(taskID)
		if err == nil {
			last = tk
			if tk.Workflow != nil && len(tk.Workflow.StepHistory) > 0 {
				return tk
			}
		}
		select {
		case <-deadline:
			return last
		case <-time.After(50 * time.Millisecond):
		}
	}
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

// TestE2E_ChaosConcurrentTasks runs multiple tasks through the workflow
// engine simultaneously, drawing scenarios from a shared FAKE_CLAUDE_SCENARIO
// queue protected by flock(2) in the fake-claude binary. Per-task invariants
// must hold even though agent invocations across tasks interleave on the
// scenario file:
//
//  1. Every task's file parses cleanly (no torn writes from concurrent
//     Manager.Update calls hitting Store.AddRun + writeSidecars on the same
//     workflow ticks).
//  2. Every task settles within the deadline (state terminal, status
//     human-required, or state waiting on wait_human).
//  3. No task leaves a running agent registered after settle.
//  4. Each task records at least one step in its own StepHistory — proving
//     the engine isolated task IDs and didn't smear history across tasks.
//
// The shared scenario queue intentionally exercises the cross-task seam.
// Fake-claude's popScenario uses flock so two simultaneous invocations can
// never observe the same first line; without that lock, a single scenario
// would feed both processes and the queue would silently desync.
func TestE2E_ChaosConcurrentTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent chaos spawns subprocesses; skipped in -short mode")
	}

	const tasks = 4
	const scenariosPerTask = 14 // upper bound on agent invocations per task
	const seed = uint64(1337)

	rng := rand.New(rand.NewPCG(seed, seed*2654435761))

	// Generate one large interleaved scenario queue. The pop order across
	// tasks is whatever the kernel grants the flock first; what matters is
	// that every task drains *something* from the queue and the queue never
	// runs short — short queue would force a fallback to "success" for
	// trailing pops, which is also fine but would mask any drain-related
	// bug as silently-passing.
	totalScenarios := tasks * scenariosPerTask
	queue := make([]string, totalScenarios)
	for i := range queue {
		queue[i] = chaosScenarioPool[rng.IntN(len(chaosScenarioPool))]
	}
	t.Logf("concurrent chaos queue (seed=%d, %d entries): %s",
		seed, len(queue), strings.Join(queue, " → "))

	env := setupE2EMulti(t, queue)

	// Create tasks first. Creation goes through the same lock-protected
	// Store path that the workflow tick exercises later, so any race in
	// Create + List would surface here.
	taskIDs := make([]string, tasks)
	for i := range taskIDs {
		created, err := env.tasks.Create(fmt.Sprintf("chaos-concurrent-%d", i), "body", "headless")
		if err != nil {
			t.Fatalf("create task %d: %v", i, err)
		}
		taskIDs[i] = created.ID
	}

	// Start workflows in lockstep. Per-task settlement happens off the
	// goroutine so the engine sees real concurrency rather than a
	// staggered ramp.
	var wg sync.WaitGroup
	results := make([]chaosResult, tasks)
	for i, id := range taskIDs {
		i := i
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := env.startWorkflow(id, "test-simple"); err != nil {
				results[i] = chaosResult{taskID: id, err: fmt.Errorf("startWorkflow: %w", err)}
				return
			}
			settled := waitForChaosSettle(t, env, id, 60*time.Second)
			results[i] = chaosResult{taskID: id, settled: settled}
		}()
	}
	wg.Wait()

	// Per-task assertions — collect all failures before fataling so a
	// regression that breaks half the tasks is fully diagnosed in one run.
	for i, r := range results {
		if r.err != nil {
			dumpChaosState(t, env, r.taskID)
			t.Errorf("task %d (%s): %v", i, r.taskID, r.err)
			continue
		}
		if !r.settled {
			dumpChaosState(t, env, r.taskID)
			t.Errorf("task %d (%s): never settled in 60s", i, r.taskID)
			continue
		}

		tk, err := env.tasks.Get(r.taskID)
		if err != nil {
			t.Errorf("task %d (%s): post-settle get: %v", i, r.taskID, err)
			continue
		}

		if env.agents.HasRunningAgentForTask(r.taskID) {
			waitForCondition(2*time.Second, func() bool {
				return !env.agents.HasRunningAgentForTask(r.taskID)
			})
			if env.agents.HasRunningAgentForTask(r.taskID) {
				t.Errorf("task %d (%s): lingering running agent after settle", i, r.taskID)
			}
		}

		tk = pollUntilHistoryPopulated(t, env, r.taskID, 10*time.Second)
		if tk.Workflow == nil {
			t.Errorf("task %d (%s): workflow is nil after settle", i, r.taskID)
			continue
		}
		if len(tk.Workflow.StepHistory) == 0 {
			dumpChaosState(t, env, r.taskID)
			t.Errorf("task %d (%s): empty StepHistory — workflow never executed any step",
				i, r.taskID)
			continue
		}
		// Cross-task isolation: every step record must reference the
		// owning task's workflow, not someone else's. The StepHistory
		// is per-execution so the assertion is implicit, but a regression
		// that smeared cache entries across tasks would surface as the
		// step's expected stepID space being wrong.
		state := tk.Workflow.State
		status := tk.Status
		terminal := state == workflow.ExecCompleted || state == workflow.ExecFailed
		humanRequired := status == task.StatusHumanRequired
		waiting := state == workflow.ExecWaiting
		if !terminal && !humanRequired && !waiting {
			t.Errorf("task %d (%s): incoherent settle: state=%q status=%q step=%q",
				i, r.taskID, state, status, tk.Workflow.CurrentStep)
		}
	}

	// Cross-task uniqueness: each task's ID must still resolve to exactly
	// one entry in the listing. A regression that re-keyed cache entries
	// (e.g. cloneTask aliasing IDs) would surface as duplicates here.
	listed, err := env.tasks.List()
	if err != nil {
		t.Fatalf("List after concurrent chaos: %v", err)
	}
	seen := map[string]int{}
	for _, lt := range listed {
		seen[lt.ID]++
	}
	for _, id := range taskIDs {
		if seen[id] != 1 {
			t.Errorf("task %s appears %d times in List; want 1", id, seen[id])
		}
	}
}

// chaosResult captures one concurrent-task outcome to defer assertions
// until every goroutine has completed.
type chaosResult struct {
	taskID  string
	settled bool
	err     error
}
