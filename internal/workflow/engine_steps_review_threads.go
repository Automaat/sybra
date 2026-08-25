package workflow

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/taskstatus"
)

const evidenceCriterionReviewThreads = "verify_review_threads"

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
//   - the PR already merged, where unanswered threads are moot and parking a
//     human on it is pure noise
//   - no thread fetcher is wired
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
	if e.pr.ThreadFetcher == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no review thread fetcher configured"}, nil
	}
	if e.prMerged(t) {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: pr already merged"}, nil
	}

	live, err := e.pr.ThreadFetcher.FetchReviewThreads(e.ctx, t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Warn("workflow.verify-review-threads.fetch", "task_id", taskID, "pr", t.PRNumber, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: fetch failed: " + err.Error()}, nil
	}

	untouched := untouchedBriefedThreads(briefed, live)
	deferred := deferredBriefedThreads(briefed, untouched, live, wfExec.Variables[PRReviewAgentLoginVar])
	if len(untouched) == 0 && len(deferred) == 0 {
		e.recordEvidence(taskID, step.ID, evidenceCriterionReviewThreads, evidence.ProofDeterministicCheck,
			0, "gh api graphql reviewThreads", fmt.Sprintf("%d briefed threads answered", len(briefed)))
		return StepOutput{StepID: step.ID, Status: "completed", Output: "review threads answered"}, nil
	}

	var reason string
	switch {
	case len(untouched) == 0:
		reason = fmt.Sprintf("%d of %d review threads answered with a deferral instead of a fix", len(deferred), len(briefed))
		if locs := untouchedLocations(deferred, live); locs != "" {
			reason += ": " + locs
		}
	default:
		reason = fmt.Sprintf("%d of %d review threads left unanswered by the fix run", len(untouched), len(briefed))
		if locs := untouchedLocations(untouched, live); locs != "" {
			reason += ": " + locs
		}
		if len(deferred) > 0 {
			reason += fmt.Sprintf("; %d more answered with a deferral instead of a fix", len(deferred))
		}
	}
	e.logger.Warn("workflow.verify-review-threads.unanswered",
		"task_id", taskID, "pr", t.PRNumber, "untouched", len(untouched), "deferred", len(deferred), "briefed", len(briefed))
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
		e.logger.Error("workflow.verify-review-threads.status", "task_id", taskID, "err", statusErr)
	}
	e.recordEvidence(taskID, step.ID, evidenceCriterionReviewThreads, evidence.ProofDeterministicCheck, 1, "", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "unanswered review threads: flipped to human-required"}, nil
}

// deferralPatterns are the ways a fix agent concedes a review comment and then
// does not act on it. A reply matching one is the failure the review policy
// names: the reviewer's point is granted, the change is postponed to a PR
// nobody opens, and the thread reads as handled to every later gate.
//
// Every pattern is anchored on word boundaries, because the same letters occur
// in ordinary descriptions of work that was done — "moved it into a separate
// process", "closed in a deferred call", "writes its own prompt". A substring
// list matched those, and each match parks a task whose fix is already pushed.
// "defer" therefore has to name what is being deferred, and "PR" has to be the
// word, not the first two letters of "process".
//
// The list stays narrow in the other direction too: each entry is a promise to
// do the work elsewhere, never a disagreement. "Invalid, here is the evidence"
// and "happy to revisit if I am reading this wrong" are legitimate replies and
// must not park a task.
var deferralPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bdefer(?:red|ring)\b[^.!?\n]{0,40}\b(?:to|this|that|the fix|for now)\b`),
	regexp.MustCompile(`(?i)\bdefer(?:red|ral)\b\s*[:;,—-]`),
	regexp.MustCompile(`(?i)\bbut\s+defer(?:red|ring)\b`),
	regexp.MustCompile(`(?i)\b(?:as|in|into)\s+a?\s*follow[\s-]?up\b`),
	regexp.MustCompile(`(?i)\bfollow[\s-]?up\s+(?:pr|issue|ticket|task|change)\b`),
	regexp.MustCompile(`(?i)\b(?:separate|another|subsequent|later|different|its own|a new)\s+(?:pr|patch|changeset|pull request)\b`),
	regexp.MustCompile(`(?i)\bout(?:side)?\s+(?:of\s+)?(?:the\s+)?scope\b`),
	regexp.MustCompile(`(?i)\b(?:leaving|left)\b[^.!?\n]{0,60}\b(?:as[\s-]is|for now)\b`),
	regexp.MustCompile(`(?i)\bas[\s-]is\s+for\s+now\b`),
	regexp.MustCompile(`(?i)\bpick(?:ing)?\s+(?:this|it|that)\s+up\s+separately\b`),
	regexp.MustCompile(`(?i)\bnot\s+(?:in|part of)\s+this\s+(?:pr|change|patch)\b`),
	regexp.MustCompile(`(?i)\b(?:filing|file|opening|open|raise|raising)\s+a\s+(?:follow[\s-]?up|ticket|issue)\b`),
}

// deferredBriefedThreads returns the briefed threads this run answered with a
// deferral instead of a fix.
//
// untouched is subtracted: a thread nobody replied to is already reported as
// unanswered, and counting it twice would name one thread as two and call an
// unanswered thread answered.
//
// agentLogin is the identity the run posts under, and an empty one disables the
// check. Authorship cannot be read off the attribution footer: Sybra's own
// review agent stamps that footer on the review comments it writes, so on a PR
// reviewed by another Sybra instance the reviewer's own text carries it. A
// missing login therefore leaves the run unverified rather than parking it on a
// comment it never wrote.
func deferredBriefedThreads(briefed, untouched []BriefedReviewThread, live []github.ReviewThread, agentLogin string) []BriefedReviewThread {
	if agentLogin == "" {
		return nil
	}
	byID := make(map[string]github.ReviewThread, len(live))
	for i := range live {
		byID[live[i].ID] = live[i]
	}
	skip := make(map[string]struct{}, len(untouched))
	for _, u := range untouched {
		skip[u.ID] = struct{}{}
	}
	var deferred []BriefedReviewThread
	for _, b := range briefed {
		if _, ok := skip[b.ID]; ok {
			continue
		}
		cur, ok := byID[b.ID]
		if !ok || cur.IsResolved || cur.IsOutdated {
			continue
		}
		if !strings.EqualFold(cur.LastAuthorLogin, agentLogin) {
			continue
		}
		if !isDeferralReply(cur.LastCommentBody) {
			continue
		}
		deferred = append(deferred, b)
	}
	return deferred
}

// isDeferralReply reports whether body postpones the fix instead of making it.
func isDeferralReply(body string) bool {
	for _, pattern := range deferralPatterns {
		if pattern.MatchString(body) {
			return true
		}
	}
	return false
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

// prMerged reports whether the PR has already merged.
//
// Deliberately not PRState.Resolved(), which is MERGED || ReadyToMerge() and
// so is true for an open PR with green CI and no conflicts. That is the
// ordinary shape of a PR under review, and the comments dispatch that writes
// the brief keys on ActionableCount alone, independent of mergeability and CI
// - so skipping on Resolved() left this gate inert on exactly the runs it
// exists to catch. The escape hatch that motivated a PR-state check at all is
// handled in pr-fix.yaml, on the agent's verdict.
//
// A fetch failure answers false: the gate then runs, which is the
// conservative direction for a check whose whole job is to catch a run that
// claimed more than it did.
func (e *Engine) prMerged(t TaskInfo) bool {
	if e.pr.StateFetcher == nil {
		return false
	}
	state, err := e.pr.StateFetcher.FetchPRState(t.ProjectID, t.PRNumber)
	if err != nil {
		return false
	}
	return state.State == "MERGED"
}
