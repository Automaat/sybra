package workflow

import (
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/taskstatus"
)

const evidenceCriterionReviewThreads = "verify_review_threads"

// fetchReviewThreads is indirected so the step's behaviour suite can run
// without a live GitHub.
var fetchReviewThreads = github.FetchReviewThreads

// execVerifyReviewThreads checks that the pr-fix agent actually answered the
// review threads it was briefed on, and is the ground truth the comments path
// otherwise lacks: the agent enumerates threads itself, so a fetch that fails
// or returns a stale list lets it address a fraction of the feedback and still
// report a clean result. The monitor then re-detects the same threads on its
// next poll and re-dispatches, until the status-bounce guard pauses the task
// and the review feedback is lost behind what reads as a scheduling fault.
//
// A briefed thread counts as untouched when it is still unresolved, still
// anchored to live code, and has gained no comment since brief time. That is
// deliberately weaker than requiring resolution: the fix-review skill never
// resolves a reviewer's thread, so gating on resolution would park every
// honest run.
//
// Skip conditions (no-op, returns "completed"):
//   - the workflow carries no brief, which is every non-comments dispatch
//   - the task has no PR or no project
//   - the PR already landed or closed, where unanswered threads are moot and
//     parking a human on a merged PR is pure noise
//   - the live fetch fails, since a transient GitHub error must not park a run
//     that may well have done its job
func (e *Engine) execVerifyReviewThreads(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	briefed := UnmarshalBriefedReviewThreads(wfExec.Variables[PRReviewThreadBriefVar])
	if len(briefed) == 0 {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no review threads briefed"}, nil
	}
	if t.PRNumber == 0 || t.ProjectID == "" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: missing pr or project"}, nil
	}
	if e.prAlreadyLanded(t) {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: pr already resolved on remote"}, nil
	}

	live, err := fetchReviewThreads(t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Warn("workflow.verify-review-threads.fetch", "task_id", taskID, "pr", t.PRNumber, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: fetch failed: " + err.Error()}, nil
	}

	untouched := untouchedBriefedThreads(briefed, live)
	if len(untouched) == 0 {
		e.recordEvidence(taskID, step.ID, evidenceCriterionReviewThreads, evidence.ProofDeterministicCheck,
			0, "gh api graphql reviewThreads", fmt.Sprintf("%d briefed threads answered", len(briefed)))
		return StepOutput{StepID: step.ID, Status: "completed", Output: "review threads answered"}, nil
	}

	reason := fmt.Sprintf("%d of %d review threads left unanswered by the fix run", len(untouched), len(briefed))
	if locs := untouchedLocations(untouched, live); locs != "" {
		reason += ": " + locs
	}
	e.logger.Warn("workflow.verify-review-threads.unanswered",
		"task_id", taskID, "pr", t.PRNumber, "untouched", len(untouched), "briefed", len(briefed))
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
		e.logger.Error("workflow.verify-review-threads.status", "task_id", taskID, "err", statusErr)
	}
	e.recordEvidence(taskID, step.ID, evidenceCriterionReviewThreads, evidence.ProofDeterministicCheck, 1, "", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "unanswered review threads: flipped to human-required"}, nil
}

// untouchedBriefedThreads returns the briefed threads the run never answered.
// A thread that vanished from the live set is treated as answered: it was
// deleted or fell outside the fetch window, neither of which is the agent
// ignoring feedback.
func untouchedBriefedThreads(briefed []BriefedReviewThread, live []github.ReviewThread) []BriefedReviewThread {
	byID := make(map[string]github.ReviewThread, len(live))
	for i := range live {
		byID[live[i].ID] = live[i]
	}
	var untouched []BriefedReviewThread
	for _, b := range briefed {
		cur, ok := byID[b.ID]
		if !ok {
			continue
		}
		if cur.IsResolved || cur.IsOutdated {
			continue
		}
		// Equality, not ">": a reviewer deleting a comment shrinks the count,
		// and a shrunk count still means the thread moved since brief time.
		// Only a thread that looks byte-for-byte untouched is held against
		// the run.
		if cur.CommentCount != b.Comments {
			continue
		}
		untouched = append(untouched, b)
	}
	return untouched
}

// untouchedLocations names up to three unanswered threads so the parked task
// says which feedback was dropped instead of only how much.
func untouchedLocations(untouched []BriefedReviewThread, live []github.ReviewThread) string {
	byID := make(map[string]github.ReviewThread, len(live))
	for i := range live {
		byID[live[i].ID] = live[i]
	}
	var locs []string
	for _, b := range untouched {
		cur, ok := byID[b.ID]
		if !ok || cur.Path == "" {
			continue
		}
		loc := cur.Path
		if cur.Line > 0 {
			loc = fmt.Sprintf("%s:%d", cur.Path, cur.Line)
		}
		locs = append(locs, loc)
		if len(locs) == 3 {
			break
		}
	}
	if len(locs) == 0 {
		return ""
	}
	if len(untouched) > len(locs) {
		return strings.Join(locs, ", ") + ", ..."
	}
	return strings.Join(locs, ", ")
}

// prAlreadyLanded reports whether the PR is merged or otherwise resolved on
// the remote. A fetch failure answers false: the gate then runs, which is the
// conservative direction for a check whose whole job is to catch a run that
// claimed more than it did.
func (e *Engine) prAlreadyLanded(t TaskInfo) bool {
	if e.pr.StateFetcher == nil {
		return false
	}
	state, err := e.pr.StateFetcher.FetchPRState(t.ProjectID, t.PRNumber)
	if err != nil {
		return false
	}
	return state.Resolved()
}
