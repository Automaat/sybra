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
	SelfAuthoredBot   bool   `json:"selfAuthoredBot"`
	CreatedAt         string `json:"createdAt"`
	UpdatedAt         string `json:"updatedAt"`
	// BaseRefName is the PR's target branch (e.g. "main"). Used to pre-flight
	// native auto-merge eligibility against the base branch's protection —
	// never the head branch.
	BaseRefName string `json:"baseRefName"`
	// AutoMergeEnabled reports whether GitHub's native auto-merge is currently
	// armed on this PR (derived live from `autoMergeRequest` being non-null —
	// never persisted on the task model).
	AutoMergeEnabled bool `json:"autoMergeEnabled"`
	// SourcedViaREST marks a PullRequest fetched over GitHub's REST API
	// (fetchPRForMonitorViaREST) instead of GraphQL, used when the GraphQL
	// budget is low. REST exposes no thread-resolution or Copilot-review data,
	// so UnresolvedCount, ActionableCount, CopilotReviewed, and ReviewDecision
	// are unset/zero on a REST-sourced PR — callers must not trust those fields
	// and must gate any REST-sourced action on RESTApproved/RESTMergeableState/
	// RESTCIFetched instead.
	SourcedViaREST bool `json:"sourcedViaRest"`
	// RESTMergeableState is the raw GitHub mergeable_state (clean|blocked|
	// behind|unstable|dirty|unknown) as reported over REST. Only "clean"
	// authorizes REST-sourced auto-merge — the coarser Mergeable enum also
	// buckets blocked/behind/unstable under MERGEABLE, which must NOT
	// authorize a REST-sourced merge.
	RESTMergeableState string `json:"restMergeableState"`
	// RESTApproved reports an explicit, current-head approval computed over
	// REST review data (fetchRESTReviews + restApproval): at least one
	// non-dismissed APPROVED review whose commit_id matches HeadSHA, and no
	// non-dismissed CHANGES_REQUESTED review. It is the only review signal the
	// REST auto-merge gate trusts.
	RESTApproved bool `json:"restApproved"`
	// RESTCIFetched reports whether both REST CI legs (check-runs and legacy
	// commit status) were fetched successfully, distinguishing "fetched,
	// genuinely no checks" (CIStatus=="") from "fetch failed" — an empty
	// CIStatus must never read as green when this is false.
	RESTCIFetched bool `json:"restCiFetched"`
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
	CheckRuns           []CheckRunInfo `json:"checkRuns"`
	WaitingForStability bool           `json:"waitingForStability"`
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
