package github

// PullRequest represents a GitHub pull request for display.
type PullRequest struct {
	Number          int      `json:"number"`
	Title           string   `json:"title"`
	URL             string   `json:"url"`
	Repository      string   `json:"repository"`
	RepoName        string   `json:"repoName"`
	Author          string   `json:"author"`
	IsDraft         bool     `json:"isDraft"`
	Labels          []string `json:"labels"`
	HeadRefName     string   `json:"headRefName"`
	CIStatus        string   `json:"ciStatus"`       // SUCCESS, FAILURE, PENDING, or ""
	ReviewDecision  string   `json:"reviewDecision"` // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, or ""
	UnresolvedCount int      `json:"unresolvedCount"`
	CreatedAt       string   `json:"createdAt"`
	UpdatedAt       string   `json:"updatedAt"`
}

// ReviewSummary contains PRs grouped by relationship to the user.
type ReviewSummary struct {
	CreatedByMe     []PullRequest `json:"createdByMe"`
	ReviewRequested []PullRequest `json:"reviewRequested"`
}
