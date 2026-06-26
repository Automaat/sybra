package github

// PullRequest represents a GitHub pull request for display.
type PullRequest struct {
	Number           int      `json:"number"`
	Title            string   `json:"title"`
	URL              string   `json:"url"`
	Repository       string   `json:"repository"`
	RepoName         string   `json:"repoName"`
	Author           string   `json:"author"`
	IsDraft          bool     `json:"isDraft"`
	Labels           []string `json:"labels"`
	HeadRefName      string   `json:"headRefName"`
	HeadSHA          string   `json:"headSha"`
	CIStatus         string   `json:"ciStatus"`         // SUCCESS, FAILURE, PENDING, or ""
	HasPendingChecks bool     `json:"hasPendingChecks"` // true when any check is still in-progress/queued
	ReviewDecision   string   `json:"reviewDecision"`   // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, or ""
	Mergeable        string   `json:"mergeable"`        // MERGEABLE, CONFLICTING, UNKNOWN, or ""
	UnresolvedCount  int      `json:"unresolvedCount"`
	// ActionableCount is the subset of unresolved threads where a reviewer
	// (human or bot) left the last comment — i.e. the ball is in the agent's
	// court. A thread the fix agent already replied to is unresolved but NOT
	// actionable, so it stops re-triggering pr-fix. This is the dispatch
	// trigger; UnresolvedCount stays the raw merge-gate signal.
	ActionableCount int `json:"actionableCount"`
	// FeedbackSig fingerprints the current reviewer feedback (the review decision
	// plus the set of unresolved thread IDs). It changes when the reviewer opens
	// a new thread but not on the agent's own replies (a replied-to thread is
	// still unresolved, so its ID stays in the set) — so the pr-fix retry budget
	// resets on new feedback and caps on stale.
	FeedbackSig       string `json:"feedbackSig"`
	ViewerHasApproved bool   `json:"viewerHasApproved"`
	CopilotReviewed   bool   `json:"copilotReviewed"` // GitHub Copilot has submitted a review
	CreatedAt         string `json:"createdAt"`
	UpdatedAt         string `json:"updatedAt"`
}

// ReviewSummary contains PRs grouped by relationship to the user.
type ReviewSummary struct {
	CreatedByMe     []PullRequest `json:"createdByMe"`
	ReviewRequested []PullRequest `json:"reviewRequested"`
	ReviewedByMe    []PullRequest `json:"reviewedByMe"`
}

// CheckRunInfo represents a single CI check run.
type CheckRunInfo struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // COMPLETED, IN_PROGRESS, QUEUED
	Conclusion string `json:"conclusion"` // SUCCESS, FAILURE, NEUTRAL, CANCELLED, TIMED_OUT
}

// RenovatePR extends PullRequest with individual check run details.
type RenovatePR struct {
	PullRequest
	CheckRuns []CheckRunInfo `json:"checkRuns"`
}

// Issue represents a GitHub issue for display.
type Issue struct {
	Number     int      `json:"number"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	Repository string   `json:"repository"`
	RepoName   string   `json:"repoName"`
	Labels     []string `json:"labels"`
	Author     string   `json:"author"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
}
