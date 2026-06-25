package sybra

import "github.com/Automaat/sybra/internal/task"

// Review phases for inbound PR-review tasks (tag `review`). Persisted on
// task.ReviewPhase and rendered as the board's PR Reviews lane badge.
const (
	// ReviewPhaseReviewing: a review agent is actively working the PR.
	ReviewPhaseReviewing = "reviewing"
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
	// the reviewed commit — the human needs a final pass and approval.
	ReviewPhaseNeedsApproval = "needs-approval"
	// ReviewPhaseApproved: the human approved; the PR is waiting to merge.
	ReviewPhaseApproved = "approved"
)

// reviewSignals are the live GitHub facts about a review task's PR, gathered
// from the review summary plus the per-PR reviews endpoint.
type reviewSignals struct {
	AgentRunning   bool   // a review agent is running for this task
	HasDraft       bool   // viewer has a PENDING (unsubmitted) review
	ViewerApproved bool   // viewer's latest submitted review is an approval
	Submitted      bool   // viewer has a submitted (non-draft) review
	ReRequested    bool   // PR is back in viewer's review-requested list
	HeadSHA        string // current PR head commit ("" if unknown)
	ReviewedSHA    string // commit the viewer's latest review was made against
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

// computeReviewPhase maps live PR signals to the desired review phase and
// status. Pure and table-tested; the poller wraps it to apply only the deltas
// (see reconcileReviewPhases).
func computeReviewPhase(s reviewSignals) reviewPhaseResult {
	// An agent owning the PR trumps everything: surface "reviewing" but leave
	// the status untouched so we don't fight triage/fix dispatch.
	if s.AgentRunning {
		return reviewPhaseResult{Phase: ReviewPhaseReviewing}
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
		// either way it needs a fresh human pass before approval.
		advanced := s.HeadSHA != "" && s.ReviewedSHA != "" && s.HeadSHA != s.ReviewedSHA
		if s.ReRequested || advanced {
			return reviewPhaseResult{
				Phase:  ReviewPhaseNeedsApproval,
				Status: task.StatusHumanRequired,
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
