package completion

import (
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/reviewprogress"
)

func TestReviewCheckpointCannotBeSalvagedAsFinalEvidence(t *testing.T) {
	manager := newMinimalTaskManager(t)
	task, err := manager.Create("review fixture", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{logger: discardLogger(), tasks: manager}
	a := &agent.Agent{ID: "review", TaskID: task.ID, Role: agent.RoleReview}
	a.SetEscalationReason("cost")
	a.AppendOutput(agent.StreamEvent{Type: "assistant", Content: reviewprogress.Start + `{"inspected":["source"],"findings":[],"remaining":["finish"]}` + reviewprogress.End})
	h.salvageInterruptedReview(a)
	got, err := manager.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CodeReview != "" || got.Reviewed || interruptedReviewAssistantTranscript(a) != "" {
		t.Fatal("partial checkpoint authorized final review evidence")
	}
}
