package recovery

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/reconcile"
)

type fixedReconciler struct {
	plan reconcile.Plan
	req  reconcile.Request
}

func (r *fixedReconciler) Reconcile(_ context.Context, req reconcile.Request) (reconcile.Plan, error) {
	r.req = req
	return r.plan, nil
}

func TestReconcileBeforeAdvanceFencesStaleRecovery(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	runner := &fixedReconciler{plan: reconcile.Plan{Action: reconcile.ActionWait, Reason: "stale lease"}}
	r := &Recovery{Logger: logger, Reconciler: runner}
	if r.reconcileBeforeAdvance(context.Background(), "task-1", "run-1", reconcile.IntentStaleRun) {
		t.Fatal("stale recovery advanced through a wait plan")
	}
	if runner.req.TaskID != "task-1" || runner.req.RunID != "run-1" || runner.req.Intent != reconcile.IntentStaleRun {
		t.Fatalf("request = %#v", runner.req)
	}
	runner.plan = reconcile.Plan{Action: reconcile.ActionAdvance, DeliverRunOutcome: true}
	if !r.reconcileBeforeAdvance(context.Background(), "task-1", "run-1", reconcile.IntentRestart) {
		t.Fatal("safe restart plan did not advance")
	}
}
