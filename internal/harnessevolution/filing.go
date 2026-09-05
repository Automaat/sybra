package harnessevolution

import (
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/task"
)

// TaskStore is the board surface filing a proposal needs.
type TaskStore interface {
	List() ([]task.Task, error)
	CreateWithStatus(title, body, mode string, status task.Status, extra task.Update) (task.Task, error)
}

// FileProposals turns each accepted proposal into a task, skipping the
// rejected ones and any that a live task already records.
//
// This lives beside the proposals rather than in the caller because both
// sybra-cli and the server file them, and a second copy of the dedupe rule
// would let one of them re-file a proposal the other had already recorded.
//
// A degraded run never populates Proposals (see finishDegraded), so it lands
// here as a no-op rather than a case to guard.
func FileProposals(store TaskStore, result RunResult) ([]task.Task, error) {
	existing, err := store.List()
	if err != nil {
		return nil, err
	}
	var filed []task.Task
	for i := range result.Proposals {
		p := result.Proposals[i]
		if p.Evaluation.Recommendation == RecommendationReject {
			continue
		}
		if _, ok := FindExistingProposal(existing, p.ID); ok {
			continue
		}
		body := RenderProposalBody(p, result.Clusters[i])
		tags := []string{"harness-proposal", string(p.Kind), string(p.Evaluation.Recommendation)}
		status := task.StatusTodo
		if p.RequiresHumanApproval || p.Evaluation.Recommendation == RecommendationNeedsHumanReview {
			tags = append(tags, "requires-human")
			status = task.StatusHumanRequired
		}
		update := task.Update{Tags: &tags}
		if status == task.StatusHumanRequired {
			update.Escalation = task.PolicyRequired("harness.proposal_approval_required", "harness proposal requires approval")
			update.AutonomyOutcome = task.HumanRequiredOutcome()
		}
		created, err := store.CreateWithStatus(p.Title, body, task.AgentModeHeadless, status, update)
		if err != nil {
			return filed, err
		}
		filed = append(filed, created)
		existing = append(existing, created)
	}
	return filed, nil
}

// FindExistingProposal reports the live task already recording a proposal.
func FindExistingProposal(tasks []task.Task, proposalID string) (task.Task, bool) {
	marker := "Proposal ID:** `" + proposalID + "`"
	for i := range tasks {
		if task.IsTerminalStatus(tasks[i].Status) {
			continue
		}
		if !slices.Contains(tasks[i].Tags, "harness-proposal") {
			continue
		}
		if strings.Contains(tasks[i].Body, marker) {
			return tasks[i], true
		}
	}
	return task.Task{}, false
}
