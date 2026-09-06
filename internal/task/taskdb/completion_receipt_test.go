package taskdb

import (
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func TestManagerRemoteReceiptPersistsWithResultAndCost(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		mgr := newTestManager(t, d)
		created, err := mgr.CreateBy("receipt fixture", "", "headless", "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.AddRunBy(created.ID, "fixture", task.AgentRun{AgentID: "run-receipt", State: string(agent.StateRunning)}); err != nil {
			t.Fatal(err)
		}
		receipt, result, state, outcome, cost := "v1:fixture", "canonical result", string(agent.StateStopped), task.RunOutcomeFailure, 2.5
		patch := task.RunPatch{RemoteCompletionReceipt: &receipt, Result: &result, State: &state, Outcome: &outcome, CostUSD: &cost}
		for range 2 {
			if err := mgr.UpdateRunBy(created.ID, "completion.record_run_result", "run-receipt", patch); err != nil {
				t.Fatal(err)
			}
		}
		// A new manager must read the durable document, not the writer's cache.
		stored, err := newTestManager(t, d).Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		run := stored.AgentRuns[0]
		if run.RemoteCompletionReceipt != receipt || run.Result != result || run.State != state || run.CostUSD != cost || run.Outcome != outcome {
			t.Fatalf("canonical completion did not round trip: %+v", run)
		}
		blank := ""
		if err := mgr.UpdateRunBy(created.ID, "fixture", "run-receipt", task.RunPatch{RemoteCompletionReceipt: &blank}); err != nil {
			t.Fatal(err)
		}
		stored, err = newTestManager(t, d).Get(created.ID)
		if err != nil || stored.AgentRuns[0].RemoteCompletionReceipt != receipt {
			t.Fatalf("blank later patch erased receipt: %+v, %v", stored, err)
		}
	})
}
