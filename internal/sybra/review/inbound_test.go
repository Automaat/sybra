package review

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestStartFixReviewAgentSkipsWhenDispatchClaimHeld guards the fix for the
// duplicate-dispatch race: StartFixReviewAgent (reached from the manual
// "Fix Review" UI action) used to call agents.Run without checking whether
// an automated fix-review dispatch already held the task's claim (e.g. via
// WorkflowEngine.DispatchEvent), so both could start a headless agent
// against the same worktree/branch concurrently. It must now respect an
// already-held claim and skip without starting a second agent.
func TestStartFixReviewAgentSkipsWhenDispatchClaimHeld(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created, err := tasks.Create("Fix PR", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	updated, err := tasks.Update(created.ID, task.Update{
		ProjectID: task.Ptr("Automaat/sybra"),
		PRNumber:  task.Ptr(7),
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	claim, ok := r.agents.TryClaimDispatch(updated.ID)
	if !ok {
		t.Fatal("claim dispatch")
	}
	defer claim.Release()

	if err := r.StartFixReviewAgent(updated); err != nil {
		t.Fatalf("StartFixReviewAgent() error = %v, want nil (skip)", err)
		panic("unreachable")
	}

	fresh, err := tasks.Get(updated.ID)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if len(fresh.AgentRuns) != 0 {
		t.Fatalf("AgentRuns = %d, want 0: a fix-review agent must not start while another dispatch claim is held", len(fresh.AgentRuns))
	}
}

// A manual-phase park asserts human-required as a "needs you" signal but names
// no reason, so the board showed a needs-you badge with nothing said — which
// reads as a bug rather than a handoff.
func TestApplyReviewPhase_ManualParkExplainsItself(t *testing.T) {
	h, tasks := newOutboundTestHandler(t)
	tk := mustReviewTask(t, tasks, "Review PR")

	h.applyReviewPhase(&tk, reviewPhaseResult{
		Phase:  ReviewPhaseManual,
		Status: task.StatusHumanRequired,
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
		panic("unreachable")
	}
	if strings.TrimSpace(got.StatusReason) == "" {
		t.Fatal("manual park left the board with a needs-you badge and no reason")
	}
}

// On a phase-only update the blank-fill must not overwrite a reason triage or
// reconciliation set — that is why computeReviewPhase returns an empty reason.
// (On a status transition the reason is cleared regardless, so there is
// nothing to preserve and the default is filled in instead.)
func TestApplyReviewPhase_ManualParkKeepsExistingReason(t *testing.T) {
	h, tasks := newOutboundTestHandler(t)
	tk := mustReviewTask(t, tasks, "Review PR keep")
	const existing = "blocked on an upstream release"
	// Park first, so applyReviewPhase sees an unchanged status and this is a
	// phase-only update — the case where preservation is meant to hold.
	if _, err := tasks.Apply(task.TransitionIntent{
		TaskID: tk.ID, ToStatus: task.StatusHumanRequired, Actor: "test",
		Extra: task.Update{
			StatusReason:    task.Ptr(existing),
			Escalation:      task.OperatorDecisionEvidence("test.manual_review", existing),
			AutonomyOutcome: task.HumanRequiredOutcome(),
		}, OperatorOverride: true,
	}); err != nil {
		t.Fatalf("seed park: %v", err)
	}
	seeded, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
		panic("unreachable")
	}
	if seeded.StatusReason != existing {
		t.Fatalf("seed did not persist: StatusReason = %q", seeded.StatusReason)
	}

	h.applyReviewPhase(&seeded, reviewPhaseResult{
		Phase:  ReviewPhaseManual,
		Status: task.StatusHumanRequired,
	})

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
		panic("unreachable")
	}
	if got.StatusReason != existing {
		t.Fatalf("StatusReason = %q, want the pre-existing %q preserved", got.StatusReason, existing)
	}
}

func mustReviewTask(t *testing.T, tasks *task.Manager, title string) task.Task {
	t.Helper()
	created, err := tasks.Create(title, "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	updated, err := tasks.Update(created.ID, task.Update{
		ProjectID: task.Ptr("Automaat/sybra"),
		PRNumber:  task.Ptr(127),
		Tags:      &[]string{"review"},
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	return updated
}
