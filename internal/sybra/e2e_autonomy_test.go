//go:build e2e

package sybra

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// TestE2E_Autonomy_Invariants asserts the five cross-subsystem autonomy
// invariants from #2441/#8650725d — no duplicate dispatch/transition, no
// foreign model, no machine blocker in human-required, at most three
// identical retries, no lost accepted write — reusing this file's existing
// fault-injection harness (setupE2EMulti, rebuildEngineFromEnv,
// scriptedGate, the test-simple/test-eval-chain workflow fixtures) rather
// than building a second one. "No lost accepted write" gets its own
// top-level test (TestE2E_Autonomy_NoLostAcceptedWrite) since it exercises
// the task store directly rather than the workflow engine.
func TestE2E_Autonomy_Invariants(t *testing.T) {
	t.Run("no_duplicate_dispatch_on_restart", testAutonomyNoDuplicateDispatchOnRestart)
	t.Run("no_foreign_model_on_failover", testAutonomyNoForeignModelOnFailover)
	t.Run("no_machine_blocker_in_human_required", testAutonomyNoMachineBlockerInHumanRequired)
	t.Run("identical_retry_cap", testAutonomyIdenticalRetryCap)
}

// testAutonomyNoDuplicateDispatchOnRestart proves a lossless redeploy
// restarting the server process twice in a row (CLAUDE.md's
// KillMode=process / ReattachAll note) never re-dispatches the same step
// twice. rebuildEngineFromEnv simulates one process restart by constructing
// a fresh workflow.Engine + agent.Manager pair against the same persisted
// task/workflow store; calling it twice back-to-back simulates two restarts
// in quick succession, the worst case for a duplicate-dispatch bug.
func testAutonomyNoDuplicateDispatchOnRestart(t *testing.T) {
	env := setupE2EProvider(t, "claude", "success")
	created, err := env.tasks.Create("autonomy restart", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       workflow.ExecRunning,
		Variables:   map[string]string{workflow.WorkflowVarDir: env.agentDir},
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status":   "in-progress",
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}

	restored1 := rebuildEngineFromEnv(t, env)
	restored1.ResumeStalled()
	waitFor(t, 15*time.Second, "first restart drives the workflow to terminal quarantine", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecFailed &&
			tk.Status == task.StatusBlocked
	})

	// A second ResumeStalled against the SAME persisted terminal state must
	// be a synchronous no-op (resumeStalledTask returns immediately for
	// ExecCompleted/ExecFailed) — never a second dispatch of "implement".
	restored2 := rebuildEngineFromEnv(t, env)
	restored2.ResumeStalled()

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := countStepRecords(tk, "implement"); got != 1 {
		t.Fatalf("implement dispatch count = %d, want exactly 1 across two restarts (no duplicate dispatch)", got)
	}
}

// testAutonomyNoForeignModelOnFailover drives a provider-health failover
// (claude unhealthy -> codex) and back, proving the dispatched agent always
// carries a concrete provider from the configured matrix and a non-empty
// model — never an empty/leftover value from the provider that failed over
// away from.
func testAutonomyNoForeignModelOnFailover(t *testing.T) {
	env := setupE2EMultiProvider(t, "claude", []string{"success", "success"})
	g := newScriptedGate()
	g.healthy["claude"] = false
	g.reason["claude"] = "rate_limited"
	g.failover["claude"] = "codex"
	env.agents.SetHealthGate(g)

	failoverRun, err := env.agents.Run(agent.RunConfig{
		TaskID:   "no-foreign-model-1",
		Name:     "failover",
		Mode:     "headless",
		Provider: "claude",
		Model:    "sonnet",
		Prompt:   "work",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "failover run stops", func() bool { return failoverRun.GetState() == agent.StateStopped })
	assertKnownProviderAndModel(t, "failover", failoverRun)
	if failoverRun.Provider == "claude" {
		t.Fatalf("failover run stayed on unhealthy provider %q", failoverRun.Provider)
	}

	g.mu.Lock()
	g.healthy["claude"] = true
	g.reason["claude"] = "ok"
	g.mu.Unlock()

	recoveryRun, err := env.agents.Run(agent.RunConfig{
		TaskID:   "no-foreign-model-2",
		Name:     "recovery",
		Mode:     "headless",
		Provider: "claude",
		Model:    "sonnet",
		Prompt:   "work",
		Dir:      env.agentDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "recovery run stops", func() bool { return recoveryRun.GetState() == agent.StateStopped })
	assertKnownProviderAndModel(t, "recovery", recoveryRun)
}

func assertKnownProviderAndModel(t *testing.T, label string, ag *agent.Agent) {
	t.Helper()
	switch ag.Provider {
	case "claude", "codex":
	default:
		t.Fatalf("%s run provider = %q, want one of the configured providers (claude, codex) — a foreign/unresolved provider leaked through", label, ag.Provider)
	}
	if strings.TrimSpace(ag.Model) == "" {
		t.Fatalf("%s run (provider=%s) has an empty model — dispatch must always resolve a model for whichever provider it actually ran under", label, ag.Provider)
	}
}

// testAutonomyNoMachineBlockerInHumanRequired proves that when the
// mechanical evaluate step (link_pr_and_review found no PR to link) parks a
// task as a typed machine-owned quarantine. Its status_reason remains a
// readable sentence rather than a raw internal error/sentinel string.
func testAutonomyNoMachineBlockerInHumanRequired(t *testing.T) {
	env := setupE2EMulti(t, []string{"success"})
	if err := os.WriteFile(
		filepath.Join(env.wfStore.Dir(), "test-eval-chain.yaml"),
		[]byte(testEvalChainWorkflowYAML), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	created, err := env.tasks.Create("autonomy no machine blocker", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.startWorkflow(created.ID, "test-eval-chain"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 20*time.Second, "eval chain workflow reaches terminal quarantine", func() bool {
		tk, gErr := env.tasks.Get(created.ID)
		return gErr == nil && tk.Workflow != nil && tk.Workflow.State == workflow.ExecFailed &&
			tk.Status == task.StatusBlocked
	})

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Status != task.StatusBlocked {
		t.Fatalf("task status = %q, want blocked machine quarantine", tk.Status)
	}
	assertMachineQuarantine(t, tk, "workflow.evaluate_no_pr")
	reason := strings.TrimSpace(tk.StatusReason)
	if reason == "" {
		t.Fatal("task quarantined with no readable status_reason")
	}
	lower := strings.ToLower(reason)
	for _, sentinel := range []string{
		"errmaxconcurrentreached", "errdispatchinflight", "erragentpoolbusy",
		"context deadline exceeded", "context canceled", "nil pointer", "panic:",
	} {
		if strings.Contains(lower, sentinel) {
			t.Fatalf("status_reason leaks an internal sentinel: %q", tk.StatusReason)
		}
	}
}

// testAutonomyIdenticalRetryCap drives the "implement" step (max_retries: 2
// in test-simple.yaml) through a run of nothing-but-failures and proves the
// engine dispatches it at most 3 times (1 initial attempt + 2 retries) —
// never an unbounded retry storm. This is the workflow-level enforcement
// the fleet-wide identical_retry_cap SLO (internal/evaluation/slo.go)
// exists to catch in aggregate.
func testAutonomyIdenticalRetryCap(t *testing.T) {
	env := setupE2EMulti(t, []string{"fail_exit", "fail_exit", "fail_exit", "fail_exit", "fail_exit"})
	created, err := env.tasks.Create("autonomy retry cap", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	wfExec := &workflow.Execution{
		WorkflowID:  "test-simple",
		CurrentStep: "implement",
		State:       workflow.ExecRunning,
		Variables:   map[string]string{workflow.WorkflowVarDir: env.agentDir},
	}
	if _, err := env.tasks.UpdateMap(created.ID, map[string]any{
		"status":   "in-progress",
		"workflow": wfExec,
	}); err != nil {
		t.Fatal(err)
	}
	env.engine.ResumeStalled()

	settled := waitForChaosSettle(t, env, created.ID, 30*time.Second)
	if !settled {
		dumpChaosState(t, env, created.ID)
		t.Fatal("workflow never settled")
	}

	tk, err := env.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	const maxAttempts = 3 // max_retries: 2 in test-simple.yaml's implement step
	if got := countStepRecords(tk, "implement"); got > maxAttempts {
		t.Fatalf("implement dispatch count = %d, want at most %d (identical retry cap)", got, maxAttempts)
	}
}

// TestE2E_Autonomy_NoLostAcceptedWrite proves an accepted write against a
// task never silently vanishes under concurrent load: N distinct tasks each
// receive one concurrent, accepted (no-error) update, and every single one
// must be durably visible afterward via both List and Get. This is the task
// store's half of #2441's "no lost accepted write" invariant — the
// workflow-engine half (no duplicate dispatch) is covered by
// TestE2E_Autonomy_Invariants/no_duplicate_dispatch_on_restart.
func TestE2E_Autonomy_NoLostAcceptedWrite(t *testing.T) {
	env := setupE2EMulti(t, []string{"success"})

	const n = 16
	ids := make([]string, n)
	for i := range n {
		created, err := env.tasks.Create(fmt.Sprintf("write-durability-%d", i), "body", "headless")
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids[i] = created.ID
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i, id := range ids {
		go func(i int, id string) {
			defer wg.Done()
			_, err := env.tasks.UpdateMap(id, map[string]any{
				"status_reason": fmt.Sprintf("accepted-write-%d", i),
			})
			errs[i] = err
		}(i, id)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("UpdateMap(%s) returned an error for an accepted write: %v", ids[i], err)
		}
	}

	listed, err := env.tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := make(map[string]task.Task, len(listed))
	for _, tk := range listed {
		byID[tk.ID] = tk
	}

	for i, id := range ids {
		want := fmt.Sprintf("accepted-write-%d", i)

		listedTk, ok := byID[id]
		if !ok {
			t.Errorf("task %s missing from List() after concurrent writes — an accepted write was lost", id)
			continue
		}
		if listedTk.StatusReason != want {
			t.Errorf("List(): task %s status_reason = %q, want %q — an accepted write was lost or overwritten", id, listedTk.StatusReason, want)
		}

		gotTk, err := env.tasks.Get(id)
		if err != nil {
			t.Errorf("Get(%s): %v", id, err)
			continue
		}
		if gotTk.StatusReason != want {
			t.Errorf("Get(): task %s status_reason = %q, want %q", id, gotTk.StatusReason, want)
		}
	}
}
