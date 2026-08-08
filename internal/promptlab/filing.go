package promptlab

import (
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/scrub"
	"github.com/Automaat/sybra/internal/task"
)

// TargetProjectID is the project a prompt proposal is filed against.
const TargetProjectID = "Automaat/sybra"

// TaskStore is the board surface filing a proposal needs.
type TaskStore interface {
	List() ([]task.Task, error)
	CreateWithStatus(title, body, mode string, status task.Status, extra task.Update) (task.Task, error)
}

// ProjectStore is the project surface the scrub and the project link need.
type ProjectStore interface {
	Get(id string) (project.Project, error)
}

// FileProposals files each proposal as a task, skipping the ones a live task
// already records within the cooldown, and scrubbing every body first.
//
// Scrubbing is explicit here rather than inherited from the caller: a proposal
// body quotes run evidence, and evidence drawn from a work-typed project
// carries that project's identifiers. Both sybra-cli and the server file these,
// so the rule lives with the proposals rather than in one of the two callers.
func FileProposals(store TaskStore, projects ProjectStore, result RunResult, cooldown time.Duration, now time.Time) ([]task.Task, error) {
	existing, err := store.List()
	if err != nil {
		return nil, err
	}
	var filed []task.Task
	for i := range result.Proposals {
		p := result.Proposals[i]
		if HasProposal(existing, p.ID, cooldown, now) {
			continue
		}
		body := ScrubProposalBody(projects, RenderProposalBody(p), p.Evidence.ProjectIDs)
		tags := []string{ProposalTag, "role:" + p.Subject.Role}
		status := task.StatusTodo
		if p.RequiresHumanApproval {
			tags = append(tags, "requires-human")
			status = task.StatusHumanRequired
		}
		update := task.Update{Tags: &tags}
		if status == task.StatusHumanRequired {
			update.Escalation = task.PolicyRequired("promptlab.approval_required", "prompt proposal requires approval")
			update.AutonomyOutcome = task.HumanRequiredOutcome()
		}
		if projectID := TargetProject(projects); projectID != "" {
			update.ProjectID = &projectID
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

// ScrubProposalBody redacts the identifiers of every work-typed project the evidence came from.
func ScrubProposalBody(projects ProjectStore, body string, projectIDs []string) string {
	if projects == nil {
		return body
	}
	for _, id := range projectIDs {
		p, err := projects.Get(id)
		if err != nil || p.Type != project.ProjectTypeWork {
			continue
		}
		if blocklist := p.WorkBlocklist(); blocklist != nil {
			body, _ = scrub.Scrub(body, blocklist)
		}
	}
	return body
}

// TargetProject returns the project to link a proposal to, or "" when it is not registered here.
func TargetProject(projects ProjectStore) string {
	if projects == nil {
		return ""
	}
	if _, err := projects.Get(TargetProjectID); err == nil {
		return TargetProjectID
	}
	return ""
}
