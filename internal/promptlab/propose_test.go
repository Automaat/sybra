package promptlab

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func proposalTask(status task.Status, tags []string, proposalID string) task.Task {
	return task.Task{
		Status: status,
		Tags:   tags,
		Body:   RenderProposalBody(Proposal{ID: proposalID, Candidate: VariantCandidate{ID: proposalID}}),
	}
}

// TestHasProposalCountsTerminalTasks pins the regression that let a single
// proposal ID be filed four separate times: dedupe skipped terminal tasks,
// so every tick re-filed a proposal whose earlier copies had aged into
// done/cancelled.
func TestHasProposalCountsTerminalTasks(t *testing.T) {
	t.Parallel()
	tags := []string{ProposalTag, "role:review"}

	for _, status := range []task.Status{
		task.StatusDone,
		task.StatusCancelled,
		task.StatusHumanRequired,
		task.StatusTodo,
		task.StatusInProgress,
	} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			tasks := []task.Task{proposalTask(status, tags, "pl-a2d853b2c1d9")}
			if !HasProposal(tasks, "pl-a2d853b2c1d9") {
				t.Fatalf("status %s: already-filed proposal must never be re-filed", status)
			}
		})
	}
}

func TestHasProposalDistinguishesIDsAndTags(t *testing.T) {
	t.Parallel()
	tags := []string{ProposalTag, "role:review"}
	tasks := []task.Task{proposalTask(task.StatusDone, tags, "pl-a2d853b2c1d9")}

	if HasProposal(tasks, "pl-5cf660095cb8") {
		t.Fatal("a different proposal ID must not match")
	}
	untagged := []task.Task{proposalTask(task.StatusDone, []string{"role:review"}, "pl-a2d853b2c1d9")}
	if HasProposal(untagged, "pl-a2d853b2c1d9") {
		t.Fatal("a task without the proposal tag must not match")
	}
	if HasProposal(nil, "pl-a2d853b2c1d9") {
		t.Fatal("empty task list must not match")
	}
}

// TestHasProposalMatchesRenderedBody guards the marker format against drifting
// from the renderer that emits it.
func TestHasProposalMatchesRenderedBody(t *testing.T) {
	t.Parallel()
	subject := WeakSubject{
		Subject:    Subject{Role: "review"},
		Metric:     "failure_rate",
		Samples:    157,
		EffectSize: 0.054,
	}
	proposals, _ := Propose([]WeakSubject{subject}, 0, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if len(proposals) == 0 {
		t.Fatal("expected a proposal")
	}
	p := proposals[0]
	filed := []task.Task{{Status: task.StatusDone, Tags: []string{ProposalTag}, Body: RenderProposalBody(p)}}
	if !HasProposal(filed, p.ID) {
		t.Fatalf("rendered body for %s must be matched by HasProposal", p.ID)
	}
}
