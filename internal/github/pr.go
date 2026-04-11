package github

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PRStats holds size metrics for a pull request.
type PRStats struct {
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changedFiles"`
}

// PRState holds the current state of a specific PR.
type PRState struct {
	State             string `json:"state"`     // OPEN, CLOSED, MERGED
	MergedAt          string `json:"mergedAt"`  // non-empty if merged
	Mergeable         string `json:"mergeable"` // MERGEABLE, CONFLICTING, UNKNOWN
	StatusCheckRollup []struct {
		State string `json:"state"` // SUCCESS, FAILURE, PENDING, ERROR, etc.
	} `json:"statusCheckRollup"`
}

// CIStatus returns a simplified CI status: SUCCESS, FAILURE, PENDING, or "".
// FAILURE takes precedence over PENDING.
func (s PRState) CIStatus() string {
	if len(s.StatusCheckRollup) == 0 {
		return ""
	}
	hasPending := false
	for _, c := range s.StatusCheckRollup {
		switch c.State {
		case "FAILURE", "ERROR":
			return "FAILURE"
		case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING", "STALE":
			hasPending = true
		}
	}
	if hasPending {
		return "PENDING"
	}
	return "SUCCESS"
}

// ReadyToMerge reports whether the PR is open, has no conflicts, and CI passes.
func (s PRState) ReadyToMerge() bool {
	return s.State == "OPEN" &&
		s.Mergeable == "MERGEABLE" &&
		(s.CIStatus() == "SUCCESS" || s.CIStatus() == "")
}

// PRFiles holds the list of files changed by a PR.
type PRFiles struct {
	Files []struct {
		Path string `json:"path"`
	} `json:"files"`
}

// PRBranch holds the head branch name of a PR.
type PRBranch struct {
	HeadRefName string `json:"headRefName"`
}

// PRContext holds review context needed when re-dispatching a PR fix agent.
type PRContext struct {
	URL      string
	Branch   string
	Comments []PRReviewComment
}

// PRReviewComment represents a single review comment on a PR.
type PRReviewComment struct {
	Author string
	Body   string
	Path   string // empty for top-level review comments
}

// FetchPR fetches a single pull request by repo (owner/repo) and number.
func FetchPR(repo string, number int) (PullRequest, error) {
	return fetchPRWith(defaultExecer, repo, number)
}

func fetchPRWith(e execer, repo string, number int) (PullRequest, error) {
	out, err := e.run("pr", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "number,title,body,url,headRefName,author,labels")
	if err != nil {
		return PullRequest{}, fmt.Errorf("gh pr view %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var raw struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return PullRequest{}, fmt.Errorf("parse pr: %w", err)
	}
	labels := make([]string, len(raw.Labels))
	for i, l := range raw.Labels {
		labels[i] = l.Name
	}
	parts := strings.SplitN(repo, "/", 2)
	repoName := ""
	if len(parts) == 2 {
		repoName = parts[1]
	}
	return PullRequest{
		Number:      raw.Number,
		Title:       raw.Title,
		URL:         raw.URL,
		HeadRefName: raw.HeadRefName,
		Repository:  repo,
		RepoName:    repoName,
		Author:      raw.Author.Login,
		Labels:      labels,
	}, nil
}

// FetchPRStats returns additions, deletions, and changed file count for a PR.
func FetchPRStats(repo string, number int) (PRStats, error) {
	return fetchPRStatsWith(defaultExecer, repo, number)
}

func fetchPRStatsWith(e execer, repo string, number int) (PRStats, error) {
	out, err := e.run("pr", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "additions,deletions,changedFiles")
	if err != nil {
		return PRStats{}, fmt.Errorf("gh pr view %d stats: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var s PRStats
	if err := json.Unmarshal(out, &s); err != nil {
		return PRStats{}, fmt.Errorf("parse pr stats: %w", err)
	}
	return s, nil
}

// FetchPRState fetches the current state of a specific PR by repo and number.
func FetchPRState(repo string, number int) (PRState, error) {
	return fetchPRStateWith(defaultExecer, repo, number)
}

func fetchPRStateWith(e execer, repo string, number int) (PRState, error) {
	out, err := e.run("pr", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "state,mergedAt,mergeable,statusCheckRollup")
	if err != nil {
		return PRState{}, fmt.Errorf("gh pr view %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var s PRState
	if err := json.Unmarshal(out, &s); err != nil {
		return PRState{}, fmt.Errorf("parse pr state: %w", err)
	}
	return s, nil
}

// FetchPRFiles returns the paths of files changed by a PR.
func FetchPRFiles(repo string, number int) ([]string, error) {
	return fetchPRFilesWith(defaultExecer, repo, number)
}

func fetchPRFilesWith(e execer, repo string, number int) ([]string, error) {
	out, err := e.run("pr", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "files")
	if err != nil {
		return nil, fmt.Errorf("gh pr view %d files: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var f PRFiles
	if err := json.Unmarshal(out, &f); err != nil {
		return nil, fmt.Errorf("parse pr files: %w", err)
	}
	paths := make([]string, len(f.Files))
	for i := range f.Files {
		paths[i] = f.Files[i].Path
	}
	return paths, nil
}

// FetchPRBranch returns the head branch name for a PR.
func FetchPRBranch(repo string, number int) (string, error) {
	return fetchPRBranchWith(defaultExecer, repo, number)
}

func fetchPRBranchWith(e execer, repo string, number int) (string, error) {
	out, err := e.run("pr", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "headRefName")
	if err != nil {
		return "", fmt.Errorf("gh pr view %d branch: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var b PRBranch
	if err := json.Unmarshal(out, &b); err != nil {
		return "", fmt.Errorf("parse pr branch: %w", err)
	}
	return b.HeadRefName, nil
}

// FetchPRContext returns the URL, branch, and unresolved review comments for a PR.
func FetchPRContext(repo string, number int) (PRContext, error) {
	return fetchPRContextWith(defaultExecer, repo, number)
}

func fetchPRContextWith(e execer, repo string, number int) (PRContext, error) {
	// Fetch PR metadata: url, branch, and review bodies
	out, err := e.run("pr", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "url,headRefName,reviews")
	if err != nil {
		return PRContext{}, fmt.Errorf("gh pr view %d context: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var meta struct {
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
		Reviews     []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body  string `json:"body"`
			State string `json:"state"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal(out, &meta); err != nil {
		return PRContext{}, fmt.Errorf("parse pr context: %w", err)
	}

	ctx := PRContext{URL: meta.URL, Branch: meta.HeadRefName}

	// Include only CHANGES_REQUESTED review bodies.
	for _, r := range meta.Reviews {
		if r.State != "CHANGES_REQUESTED" || strings.TrimSpace(r.Body) == "" {
			continue
		}
		ctx.Comments = append(ctx.Comments, PRReviewComment{
			Author: r.Author.Login,
			Body:   strings.TrimSpace(r.Body),
		})
	}

	// Fetch inline diff comments (unresolved review thread comments).
	inlineOut, err := e.run("api",
		fmt.Sprintf("repos/%s/pulls/%d/comments", repo, number),
		"-q", `.[] | select(.position != null) | {author: .user.login, body: .body, path: .path}`)
	if err == nil && len(inlineOut) > 0 {
		for line := range strings.SplitSeq(strings.TrimSpace(string(inlineOut)), "\n") {
			if line == "" {
				continue
			}
			var c struct {
				Author string `json:"author"`
				Body   string `json:"body"`
				Path   string `json:"path"`
			}
			if jsonErr := json.Unmarshal([]byte(line), &c); jsonErr != nil {
				continue
			}
			if strings.TrimSpace(c.Body) == "" {
				continue
			}
			ctx.Comments = append(ctx.Comments, PRReviewComment{
				Author: c.Author,
				Body:   strings.TrimSpace(c.Body),
				Path:   c.Path,
			})
		}
	}

	return ctx, nil
}

// FetchPRClosingIssues returns the issue numbers GitHub parses from
// the PR body as closing (via keywords like "Closes #N", "Fixes #N"),
// restricted to same-repo references, plus the current PR body.
// Cross-repo closing references are ignored by the filter so callers
// can compare numbers directly against their own repo context.
func FetchPRClosingIssues(repo string, number int) (issues []int, body string, err error) {
	return fetchPRClosingIssuesWith(defaultExecer, repo, number)
}

func fetchPRClosingIssuesWith(e execer, repo string, number int) (issues []int, body string, err error) {
	out, runErr := e.run("pr", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "closingIssuesReferences,body")
	if runErr != nil {
		return nil, "", fmt.Errorf("gh pr view %d: %s: %w", number, strings.TrimSpace(string(out)), runErr)
	}
	var raw struct {
		Body                    string `json:"body"`
		ClosingIssuesReferences []struct {
			Number     int `json:"number"`
			Repository struct {
				Name  string `json:"name"`
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repository"`
		} `json:"closingIssuesReferences"`
	}
	if jsonErr := json.Unmarshal(out, &raw); jsonErr != nil {
		return nil, "", fmt.Errorf("parse pr closing issues: %w", jsonErr)
	}
	parts := strings.SplitN(repo, "/", 2)
	var wantOwner, wantName string
	if len(parts) == 2 {
		wantOwner, wantName = parts[0], parts[1]
	}
	for _, ref := range raw.ClosingIssuesReferences {
		// Accept any ref whose repository matches the PR repo, or refs
		// with empty repository metadata (older gh versions).
		refOwner := ref.Repository.Owner.Login
		refName := ref.Repository.Name
		if (refOwner == "" && refName == "") || (refOwner == wantOwner && refName == wantName) {
			issues = append(issues, ref.Number)
		}
	}
	return issues, raw.Body, nil
}

// MergePR merges a pull request using squash strategy.
func MergePR(repo string, number int) error {
	return mergePRWith(defaultExecer, repo, number)
}

func mergePRWith(e execer, repo string, number int) error {
	out, err := e.run("pr", "merge", fmt.Sprintf("%d", number),
		"--repo", repo, "--squash")
	if err != nil {
		return fmt.Errorf("gh pr merge %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// MarkReady marks a draft pull request as ready for review.
func MarkReady(repo string, number int) error {
	return markReadyWith(defaultExecer, repo, number)
}

func markReadyWith(e execer, repo string, number int) error {
	out, err := e.run("pr", "ready", fmt.Sprintf("%d", number), "-R", repo)
	if err != nil {
		return fmt.Errorf("gh pr ready: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// EditPRBody replaces the body of a pull request.
func EditPRBody(repo string, number int, body string) error {
	return editPRBodyWith(defaultExecer, repo, number, body)
}

func editPRBodyWith(e execer, repo string, number int, body string) error {
	out, err := e.run("pr", "edit", fmt.Sprintf("%d", number),
		"--repo", repo, "--body", body)
	if err != nil {
		return fmt.Errorf("gh pr edit %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}
