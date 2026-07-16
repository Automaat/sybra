package promptlab

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

var (
	refNow   = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cooldown = 30 * 24 * time.Hour
)

func proposalTask(status task.Status, tags []string, proposalID string, updatedAt time.Time) task.Task {
	return task.Task{
		Status:    status,
		Tags:      tags,
		UpdatedAt: updatedAt,
		Body:      RenderProposalBody(Proposal{ID: proposalID, Candidate: VariantCandidate{ID: proposalID}}),
	}
}

// TestHasProposalSuppressesWithinCooldown pins the four-duplicates regression:
// dedupe skipped terminal tasks entirely, so a proposal whose earlier copies
// had aged into done/cancelled was re-filed on every tick.
func TestHasProposalSuppressesWithinCooldown(t *testing.T) {
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
			tasks := []task.Task{proposalTask(status, tags, "pl-a2d853b2c1d9", refNow.Add(-time.Hour))}
			if !HasProposal(tasks, "pl-a2d853b2c1d9", cooldown, refNow) {
				t.Fatalf("status %s: a proposal filed an hour ago must not be re-filed", status)
			}
		})
	}
}

// TestHasProposalRefilesTerminalAfterCooldown pins the other half: suppressing
// on terminal tasks forever would cap each subject at two proposals for the
// life of the board (two intents per subject) and then go permanently silent,
// even while the role stays weak.
func TestHasProposalRefilesTerminalAfterCooldown(t *testing.T) {
	t.Parallel()
	tags := []string{ProposalTag, "role:review"}
	stale := refNow.Add(-cooldown - time.Hour)

	for _, status := range []task.Status{task.StatusDone, task.StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			tasks := []task.Task{proposalTask(status, tags, "pl-a2d853b2c1d9", stale)}
			if HasProposal(tasks, "pl-a2d853b2c1d9", cooldown, refNow) {
				t.Fatalf("status %s: a subject must be re-proposable once cooldown elapses", status)
			}
		})
	}
}

// TestHasProposalLiveProposalIgnoresCooldown pins that an in-flight proposal
// suppresses regardless of age — cooldown governs re-proposing a decided
// subject, not spawning a second copy of one still being worked.
func TestHasProposalLiveProposalIgnoresCooldown(t *testing.T) {
	t.Parallel()
	tags := []string{ProposalTag, "role:review"}
	ancient := refNow.Add(-10 * cooldown)

	for _, status := range []task.Status{task.StatusHumanRequired, task.StatusTodo, task.StatusInProgress} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			tasks := []task.Task{proposalTask(status, tags, "pl-a2d853b2c1d9", ancient)}
			if !HasProposal(tasks, "pl-a2d853b2c1d9", cooldown, refNow) {
				t.Fatalf("status %s: a live proposal must suppress at any age", status)
			}
		})
	}
}

func TestHasProposalZeroCooldownNeverRefiles(t *testing.T) {
	t.Parallel()
	tags := []string{ProposalTag, "role:review"}
	ancient := refNow.Add(-10 * cooldown)
	tasks := []task.Task{proposalTask(task.StatusDone, tags, "pl-a2d853b2c1d9", ancient)}

	if !HasProposal(tasks, "pl-a2d853b2c1d9", 0, refNow) {
		t.Fatal("zero cooldown must suppress on any terminal task")
	}
}

func TestHasProposalDistinguishesIDsAndTags(t *testing.T) {
	t.Parallel()
	tags := []string{ProposalTag, "role:review"}
	tasks := []task.Task{proposalTask(task.StatusDone, tags, "pl-a2d853b2c1d9", refNow)}

	if HasProposal(tasks, "pl-5cf660095cb8", cooldown, refNow) {
		t.Fatal("a different proposal ID must not match")
	}
	untagged := []task.Task{proposalTask(task.StatusDone, []string{"role:review"}, "pl-a2d853b2c1d9", refNow)}
	if HasProposal(untagged, "pl-a2d853b2c1d9", cooldown, refNow) {
		t.Fatal("a task without the proposal tag must not match")
	}
	if HasProposal(nil, "pl-a2d853b2c1d9", cooldown, refNow) {
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
	proposals, _ := Propose([]WeakSubject{subject}, 0, refNow)
	if len(proposals) == 0 {
		t.Fatal("expected a proposal")
	}
	p := proposals[0]
	filed := []task.Task{{
		Status:    task.StatusHumanRequired,
		Tags:      []string{ProposalTag},
		UpdatedAt: refNow,
		Body:      RenderProposalBody(p),
	}}
	if !HasProposal(filed, p.ID, cooldown, refNow) {
		t.Fatalf("rendered body for %s must be matched by HasProposal", p.ID)
	}
}
