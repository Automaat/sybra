package review

import (
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
	}
	updated, err := tasks.Update(created.ID, task.Update{
		ProjectID: task.Ptr("Automaat/sybra"),
		PRNumber:  task.Ptr(7),
	})
	if err != nil {
		t.Fatal(err)
	}

	claim, ok := r.agents.TryClaimDispatch(updated.ID)
	if !ok {
		t.Fatal("claim dispatch")
	}
	defer claim.Release()

	if err := r.StartFixReviewAgent(updated); err != nil {
		t.Fatalf("StartFixReviewAgent() error = %v, want nil (skip)", err)
	}

	fresh, err := tasks.Get(updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.AgentRuns) != 0 {
		t.Fatalf("AgentRuns = %d, want 0: a fix-review agent must not start while another dispatch claim is held", len(fresh.AgentRuns))
	}
}
