package completion

import (
	"context"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/reconcile"
)

type recordingReconciler struct {
	plan reconcile.Plan
	req  reconcile.Request
}

func (r *recordingReconciler) Reconcile(_ context.Context, req reconcile.Request) (reconcile.Plan, error) {
	r.req = req
	return r.plan, nil
}

func TestAuthorCompletionPassesThroughReconciler(t *testing.T) {
	t.Parallel()
	r := &recordingReconciler{plan: reconcile.Plan{Action: reconcile.ActionAdvance, DeliverRunOutcome: true}}
	h := New(Config{Logger: discardLogger(), Reconciler: r})
	ag := &agent.Agent{ID: "run-1", TaskID: "task-1", Role: agent.RoleImplementation}
	if !h.reconcileAuthorCompletion(ag, false) {
		t.Fatal("safe reconciliation did not allow workflow advancement")
	}
	if r.req.TaskID != ag.TaskID || r.req.RunID != ag.ID || r.req.Intent != reconcile.IntentAuthorCompletion {
		t.Fatalf("request = %#v", r.req)
	}
}

func TestAuthorCompletionStartsRepairInsteadOfAdvancing(t *testing.T) {
	t.Parallel()
	r := &recordingReconciler{plan: reconcile.Plan{Action: reconcile.ActionRepair, Reason: "merge in progress"}}
	started := false
	h := New(Config{Logger: discardLogger(), Reconciler: r, ConflictRecovery: func(string) bool {
		started = true
		return true
	}})
	if h.reconcileAuthorCompletion(&agent.Agent{ID: "run-1", TaskID: "task-1", Role: agent.RoleImplementation}, false) {
		t.Fatal("repair plan advanced the unresolved author run")
	}
	if !started {
		t.Fatal("repair plan did not start bounded conflict recovery")
	}
}

func TestAuthorCompletionWaitsInsteadOfAdvancing(t *testing.T) {
	t.Parallel()
	r := &recordingReconciler{plan: reconcile.Plan{Action: reconcile.ActionWait, Reason: "stale lease"}}
	h := New(Config{Logger: discardLogger(), Reconciler: r})
	if h.reconcileAuthorCompletion(&agent.Agent{ID: "run-1", TaskID: "task-1", Role: agent.RolePRFix}, false) {
		t.Fatal("wait plan allowed workflow advancement")
	}
	if r.req.Intent != reconcile.IntentPRFix {
		t.Fatalf("intent = %q, want %q", r.req.Intent, reconcile.IntentPRFix)
	}
}

func TestStalledAuthorWaitStillReachesRetryRouting(t *testing.T) {
	t.Parallel()
	r := &recordingReconciler{plan: reconcile.Plan{Action: reconcile.ActionWait, Reason: "run is non-terminal"}}
	h := New(Config{Logger: discardLogger(), Reconciler: r})
	if !h.reconcileAuthorCompletion(&agent.Agent{ID: "run-1", TaskID: "task-1", Role: agent.RoleImplementation}, true) {
		t.Fatal("stalled run did not continue to retry routing after reconciliation")
	}
}

func TestHumanRecoveryObservesButKeepsVerdictAuthority(t *testing.T) {
	t.Parallel()
	r := &recordingReconciler{plan: reconcile.Plan{Action: reconcile.ActionWait}}
	h := New(Config{Logger: discardLogger(), Reconciler: r})
	if !h.reconcileAuthorCompletion(&agent.Agent{ID: "run-1", TaskID: "task-1", Role: agent.RoleHumanReview}, false) {
		t.Fatal("reconciler replaced human-review verdict routing")
	}
	if r.req.Intent != reconcile.IntentHumanRecovery {
		t.Fatalf("intent = %q, want %q", r.req.Intent, reconcile.IntentHumanRecovery)
	}
}
