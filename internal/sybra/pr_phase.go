package sybra

// PR phases for outbound own-PR tasks (status in-review/ready-review, not tag
// `review`). Persisted on task.PRPhase and rendered as the phase glyph on the
// board's In Review cards. Unlike the inbound review machine (review_phase.go),
// these phases never change task.Status — own-PR tasks must stay in the In
// Review column; the phase is a pure overlay.
const (
	// PRPhaseDraft: the PR is still a draft. The human must mark it ready.
	PRPhaseDraft = "draft"
	// PRPhaseBuilding: CI is still running — nothing to do but wait.
	PRPhaseBuilding = "building"
	// PRPhaseFixing: an agent is (or is about to be) auto-fixing the PR — a CI
	// failure, a merge conflict, or review comments.
	PRPhaseFixing = "fixing"
	// PRPhaseChangesRequested: reviewers left changes/unresolved comments; a
	// fix-review agent is dispatched to address them.
	PRPhaseChangesRequested = "changes-requested"
	// PRPhaseAwaitingApproval: the PR is clean and green but not yet approved —
	// the human may want to ping reviewers.
	PRPhaseAwaitingApproval = "awaiting-approval"
	// PRPhaseApproved: the PR is approved and waiting to merge (pet projects
	// auto-merge; the human merges the rest).
	PRPhaseApproved = "approved"
)

// prSignals are the live GitHub facts about an own-PR task's pull request,
// taken straight from the monitored github.PullRequest plus whether an agent is
// running for the task.
type prSignals struct {
	AgentRunning     bool   // an agent is running for this task (fix/impl)
	IsDraft          bool   // PR is a draft
	CIStatus         string // SUCCESS, FAILURE, PENDING, or ""
	HasPendingChecks bool   // a check is still in-progress/queued
	Mergeable        string // MERGEABLE, CONFLICTING, UNKNOWN, or ""
	ReviewDecision   string // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, or ""
	UnresolvedCount  int    // unresolved review threads (raw — drives merge gate)
	ActionableCount  int    // unresolved threads a reviewer last touched (drives fix dispatch)
}

// computePRPhase maps live PR signals to the desired phase. Pure and
// table-tested; the poller wraps it to apply only the deltas (see
// reconcilePRPhases). First match wins — the order encodes priority.
func computePRPhase(s prSignals) string {
	switch {
	// An agent actively working the PR trumps everything.
	case s.AgentRunning:
		return PRPhaseFixing
	// CI failed or the branch conflicts: the monitor dispatches a fix agent,
	// so surface "fixing" even in the brief gap before it spins up.
	case s.CIStatus == "FAILURE" && !s.HasPendingChecks:
		return PRPhaseFixing
	case s.Mergeable == "CONFLICTING":
		return PRPhaseFixing
	// Still a draft (and not actively being fixed for CI/conflict): the human
	// must mark it ready. This outranks the comment check because the monitor
	// skips comment-fix dispatch on drafts (MatchTaskPRs gates on !IsDraft), so
	// a draft with comments would otherwise imply a fix that never runs and
	// hide the "mark ready" action.
	case s.IsDraft:
		return PRPhaseDraft
	// Reviewers asked for changes (or left actionable threads — a reviewer had
	// the last word): a fix-review agent is dispatched to address them. Threads
	// the agent already replied to are unresolved but not actionable, so they
	// surface as awaiting-approval (waiting on the reviewer), not changes.
	case s.ReviewDecision == "CHANGES_REQUESTED" || s.ActionableCount > 0:
		return PRPhaseChangesRequested
	// CI is still running — wait it out.
	case s.CIStatus == "PENDING" || s.HasPendingChecks:
		return PRPhaseBuilding
	// Approved and clean: ready to merge.
	case s.ReviewDecision == "APPROVED":
		return PRPhaseApproved
	// Clean, green, not draft, not yet approved: waiting on reviewers.
	default:
		return PRPhaseAwaitingApproval
	}
}
