package github

import "strconv"

// PRIssueKind identifies what's wrong with a PR.
type PRIssueKind string

const (
	PRIssueConflict PRIssueKind = "conflict"
	// PRIssueBranchConflictNoPR is a tracker-only kind for branch-conflict
	// recovery before a task has opened a PR. It is never emitted by PR monitor
	// matching and exists only to keep its retry budget separate from PR-backed
	// conflicts.
	PRIssueBranchConflictNoPR PRIssueKind = "branch_conflict_no_pr"
	PRIssueBranchRecreate     PRIssueKind = "branch_recreate"
	PRIssueCIFailure          PRIssueKind = "ci_failure"
	PRIssueComments           PRIssueKind = "comments"
	PRIssueReadyToMerge       PRIssueKind = "ready_to_merge"
)

// PRIssue represents a detected problem on a PR linked to a task.
type PRIssue struct {
	Kind   PRIssueKind
	TaskID string
	PR     PullRequest
}

// TaskMatcher is the minimal task info needed for PR matching.
type TaskMatcher struct {
	ID        string
	PRNumber  int
	Branch    string
	ProjectID string // owner/repo, used for closed/merged PR detection
}

// ClosedPR represents a PR linked to a task that is merged or closed.
type ClosedPR struct {
	TaskID   string
	PRNumber int
	State    string // "MERGED" or "CLOSED"
}

// DetectClosedTaskPRs finds tasks whose linked PRs are no longer open.
// Requires TaskMatcher.ProjectID and PRNumber to be set.
// It skips tasks whose PR still appears in openPRs.
func DetectClosedTaskPRs(openPRs []PullRequest, tasks []TaskMatcher, fetchState func(repo string, number int) (PRState, error)) []ClosedPR {
	openByNumber := make(map[int]struct{}, len(openPRs))
	openByRepoNumber := make(map[string]struct{}, len(openPRs))
	for i := range openPRs {
		if openPRs[i].Repository == "" {
			openByNumber[openPRs[i].Number] = struct{}{}
			continue
		}
		openByRepoNumber[prRepoNumberKey(openPRs[i].Repository, openPRs[i].Number)] = struct{}{}
	}

	var closed []ClosedPR
	for i := range tasks {
		t := &tasks[i]
		if t.PRNumber == 0 || t.ProjectID == "" {
			continue
		}
		if _, isOpen := openByRepoNumber[prRepoNumberKey(t.ProjectID, t.PRNumber)]; isOpen {
			continue
		}
		if _, isOpen := openByNumber[t.PRNumber]; isOpen {
			continue
		}
		state, err := fetchState(t.ProjectID, t.PRNumber)
		if err != nil {
			continue
		}
		if state.State == "MERGED" || state.State == "CLOSED" {
			closed = append(closed, ClosedPR{TaskID: t.ID, PRNumber: t.PRNumber, State: state.State})
		}
	}
	return closed
}

func prRepoNumberKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// MatchTaskPRs finds issues on PRs that are linked to tasks.
// Matches by PRNumber or Branch (HeadRefName). Skips drafts and UNKNOWN mergeable.
// MatchTaskPRIndex maps task ID to the PR matched for it, using the same
// number-then-branch resolution as MatchTaskPRs. Callers need the live PR even
// when it produced no issue this cycle — "no issue" and "issue resolved" are
// different claims.
func MatchTaskPRIndex(prs []PullRequest, tasks []TaskMatcher) map[string]PullRequest {
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
	index := make(map[string]PullRequest, len(tasks))
	for i := range prs {
		pr := &prs[i]
		tm := byNumber[pr.Number]
		if tm == nil {
			tm = byBranch[pr.HeadRefName]
		}
		if tm == nil {
			continue
		}
		index[tm.ID] = *pr
	}
	return index
}

// PRIssueIndeterminate reports whether pr's live data cannot yet answer whether
// kind is resolved. A fix agent's own push or rerun puts checks in flight, which
// makes ci_failure unknowable, and GitHub recomputes mergeability
// asynchronously, which makes conflict unknowable. Reading either absence as
// "resolved" cancels the very agent that is fixing the PR.
func PRIssueIndeterminate(pr PullRequest, kind PRIssueKind) bool {
	switch kind {
	case PRIssueCIFailure:
		return pr.HasPendingChecks || pr.CIStatus == "PENDING"
	case PRIssueConflict:
		return pr.Mergeable == "" || pr.Mergeable == "UNKNOWN"
	default:
		return false
	}
}

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
		if pr.CIStatus == "FAILURE" && !pr.HasPendingChecks {
			issues = append(issues, PRIssue{Kind: PRIssueCIFailure, TaskID: tm.ID, PR: *pr})
		}
		// Dispatch a fix-review agent only for unresolved threads where a
		// reviewer had the last word. GitHub's reviewDecision can remain
		// CHANGES_REQUESTED after every thread was answered or resolved, and the
		// fix-review skill has no live thread left to process in that state.
		// Draft PRs still get review feedback from Copilot/humans; fix those
		// comments before the author marks the PR ready.
		if pr.ActionableCount > 0 {
			issues = append(issues, PRIssue{Kind: PRIssueComments, TaskID: tm.ID, PR: *pr})
		}
		if !pr.IsDraft && !pr.HasPendingChecks && pr.Mergeable == "MERGEABLE" && (pr.CIStatus == "SUCCESS" || pr.CIStatus == "") {
			issues = append(issues, PRIssue{Kind: PRIssueReadyToMerge, TaskID: tm.ID, PR: *pr})
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
