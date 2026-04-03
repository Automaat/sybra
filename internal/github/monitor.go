package github

// PRIssueKind identifies what's wrong with a PR.
type PRIssueKind string

const (
	PRIssueConflict  PRIssueKind = "conflict"
	PRIssueCIFailure PRIssueKind = "ci-failure"
)

// PRIssue represents a detected problem on a PR linked to a task.
type PRIssue struct {
	Kind   PRIssueKind
	TaskID string
	PR     PullRequest
}

// TaskMatcher is the minimal task info needed for PR matching.
type TaskMatcher struct {
	ID       string
	PRNumber int
	Branch   string
}

// MatchTaskPRs finds issues on PRs that are linked to tasks.
// Matches by PRNumber or Branch (HeadRefName). Skips drafts and UNKNOWN mergeable.
func MatchTaskPRs(prs []PullRequest, tasks []TaskMatcher) []PRIssue {
	byNumber := make(map[int]*TaskMatcher, len(tasks))
	byBranch := make(map[string]*TaskMatcher, len(tasks))
	for i := range tasks {
		if tasks[i].PRNumber > 0 {
			byNumber[tasks[i].PRNumber] = &tasks[i]
		}
		if tasks[i].Branch != "" {
			byBranch[tasks[i].Branch] = &tasks[i]
		}
	}

	var issues []PRIssue
	for i := range prs {
		pr := &prs[i]

		tm := byNumber[pr.Number]
		if tm == nil {
			tm = byBranch[pr.HeadRefName]
		}
		if tm == nil {
			continue
		}

		if pr.Mergeable == "CONFLICTING" {
			issues = append(issues, PRIssue{Kind: PRIssueConflict, TaskID: tm.ID, PR: *pr})
		}
		if pr.CIStatus == "FAILURE" {
			issues = append(issues, PRIssue{Kind: PRIssueCIFailure, TaskID: tm.ID, PR: *pr})
		}
	}
	return issues
}

// ReviewPRMatch represents a review-requested PR that matches a known project.
type ReviewPRMatch struct {
	PR        PullRequest
	ProjectID string
}

// ProjectMatcher holds minimal project info for review PR matching.
type ProjectMatcher struct {
	ID         string
	Repository string // owner/repo format
}

// MatchReviewPRs identifies review-requested PRs related to known projects.
// Returns matches but takes no action — placeholder for future automation.
func MatchReviewPRs(prs []PullRequest, projects []ProjectMatcher) []ReviewPRMatch {
	byRepo := make(map[string]*ProjectMatcher, len(projects))
	for i := range projects {
		byRepo[projects[i].Repository] = &projects[i]
	}

	var matches []ReviewPRMatch
	for i := range prs {
		if pm := byRepo[prs[i].Repository]; pm != nil {
			matches = append(matches, ReviewPRMatch{PR: prs[i], ProjectID: pm.ID})
		}
	}
	return matches
}
