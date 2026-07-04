package review

import "github.com/Automaat/sybra/internal/task"

// Review phases for inbound PR-review tasks (tag `review`). Persisted on
// task.ReviewPhase and rendered as the board's PR Reviews lane badge.
const (
	// ReviewPhaseReviewing: a review agent is actively working the PR.
	ReviewPhaseReviewing = "reviewing"
	// ReviewPhaseConflict: the PR has merge conflicts and can't land until the
	// author rebases. Passive — the lane sinks it to the bottom; the human waits
	// on the author, so it never asserts the needs-you (human-required) signal.
	ReviewPhaseConflict = "conflict"
	// ReviewPhaseManual: the PR was too small for an agent and was punted to
	// the human to review by hand.
	ReviewPhaseManual = "manual"
	// ReviewPhaseDrafted: a PENDING (draft) review sits on the PR, waiting for
	// the human to verify and submit it on GitHub.
	ReviewPhaseDrafted = "drafted"
	// ReviewPhaseAwaitingAuthor: the human submitted the review; the ball is in
	// the PR author's court to push changes / respond.
	ReviewPhaseAwaitingAuthor = "awaiting-author"
	// ReviewPhaseNeedsApproval: the author re-requested review or pushed past
	// the reviewed commit. This is a visible reviewer action, but it must not
	// flip the task to human-required: the one automatic review round is already
	// spent, so the follow-up is manual approval only.
	ReviewPhaseNeedsApproval = "needs-approval"
	// ReviewPhaseApproved: the human approved; the PR is waiting to merge.
	ReviewPhaseApproved = "approved"
)

// reviewSignals are the live GitHub facts about a review task's PR, gathered
// from the review summary plus the per-PR reviews endpoint.
type reviewSignals struct {
	AgentRunning              bool   // a review agent is running for this task
	HasDraft                  bool   // viewer has a PENDING (unsubmitted) review
	ViewerApproved            bool   // viewer's latest submitted review is an approval
	Submitted                 bool   // viewer has a submitted (non-draft) review
	ReRequested               bool   // PR is back in viewer's review-requested list
	HeadSHA                   string // current PR head commit ("" if unknown)
	ReviewedSHA               string // commit the viewer's latest review was made against
	HeadLineageUnknown        bool   // head changed, but GitHub lineage lookup failed
	BaseOnlyMergeFromReviewed bool   // head only merged base into the reviewed commit
	Mergeable                 string // MERGEABLE, CONFLICTING, UNKNOWN, or ""
}

// reviewPhaseResult is the desired task state for a review task.
//
// Status is "" when the phase must not touch task.Status (e.g. while an agent
// runs).
type reviewPhaseResult struct {
	Phase  string
	Status task.Status
	Reason string
}

// stickyConflictPhase applies hysteresis to GitHub's noisy mergeability signal
// so a conflicting PR can't flap out of the conflict phase. GitHub recomputes
// mergeability asynchronously and reports UNKNOWN (or "") while a PR's base
// branch is moving, so a genuinely-conflicting PR briefly looks non-conflicting
// on any poll that catches that window. Conflict is the lane's most actionable
// fact — the PR can't land until the author rebases — so it's sticky: only a
// definitive MERGEABLE clears it.
//
// decided is true when mergeability settles the phase (the caller applies res
// and stops); false means "not conflicting — fall through to the viewer-state
// phase computation".
func stickyConflictPhase(mergeable, currentPhase string) (res reviewPhaseResult, decided bool) {
	switch mergeable {
	case "CONFLICTING":
		return computeReviewPhase(reviewSignals{Mergeable: "CONFLICTING"}), true
	case "MERGEABLE":
		// The only definitive non-conflict: clears any prior conflict and falls
		// through to the viewer-state phase computation.
		return reviewPhaseResult{}, false
	default:
		// Indeterminate read — UNKNOWN, "", or any unexpected/new state GitHub
		// might return: hold an existing conflict rather than flapping out of it.
		// If the task isn't already conflict, let viewer state win.
		if currentPhase == ReviewPhaseConflict {
			return computeReviewPhase(reviewSignals{Mergeable: "CONFLICTING"}), true
		}
		return reviewPhaseResult{}, false
	}
}

// computeReviewPhase maps live PR signals to the desired review phase and
// status. Pure and table-tested; the poller wraps it to apply only the deltas
// (see reconcileReviewPhases).
func computeReviewPhase(s reviewSignals) reviewPhaseResult {
	// An agent owning the PR trumps everything: surface "reviewing" but leave
	// the status untouched so we don't fight triage/fix dispatch.
	if s.AgentRunning {
		return reviewPhaseResult{Phase: ReviewPhaseReviewing}
	}

	// A conflicting PR can't land until the author rebases, so reviewing or
	// approving it is wasted effort — surface "conflict" and let the lane sink
	// it to the bottom. Outranks every viewer-state phase (drafted, approved,
	// awaiting-author, manual): the conflict is the actionable fact. Status stays
	// in-review so it never lights the needs-you accent — the ball is the
	// author's, not the reviewer's.
	if s.Mergeable == "CONFLICTING" {
		return reviewPhaseResult{
			Phase:  ReviewPhaseConflict,
			Status: task.StatusInReview,
			Reason: "PR has merge conflicts — author must rebase",
		}
	}

	if s.ViewerApproved {
		return reviewPhaseResult{
			Phase:  ReviewPhaseApproved,
			Status: task.StatusInReview,
			Reason: "Approved — awaiting merge",
		}
	}

	if s.HasDraft {
		return reviewPhaseResult{
			Phase:  ReviewPhaseDrafted,
			Status: task.StatusHumanRequired,
			Reason: "Draft review ready — verify & submit on GitHub",
		}
	}

	if s.Submitted {
		// The author pushed past the commit we reviewed, or re-requested review:
		// either way it needs a fresh human pass before approval. Keep the task
		// in-review so the human-required auto-diagnostic machinery does not
		// start another review round.
		advanced := reviewAdvancedPastReviewed(s)
		if s.ReRequested || advanced {
			return reviewPhaseResult{
				Phase:  ReviewPhaseNeedsApproval,
				Status: task.StatusInReview,
				Reason: "Author updated PR — do a final review & approve",
			}
		}
		return reviewPhaseResult{
			Phase:  ReviewPhaseAwaitingAuthor,
			Status: task.StatusInReview,
			Reason: "Awaiting author response",
		}
	}

	// No agent, no draft, no submitted review: the human owns it (typically a
	// small PR punted by triage). Assert human-required for the needs-you
	// signal, but emit no reason so applyReviewPhase keeps whatever reason
	// triage already set (e.g. "PR too small for agent review").
	return reviewPhaseResult{Phase: ReviewPhaseManual, Status: task.StatusHumanRequired}
}

func reviewAdvancedPastReviewed(s reviewSignals) bool {
	if s.HeadSHA == "" || s.ReviewedSHA == "" || s.HeadSHA == s.ReviewedSHA {
		return false
	}
	if s.HeadLineageUnknown || s.BaseOnlyMergeFromReviewed {
		return false
	}
	return true
}
