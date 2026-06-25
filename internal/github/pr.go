package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// mergeRetryDelays controls the backoff between merge retries when GitHub
// reports the base branch was modified mid-merge. Overridable from tests.
var mergeRetryDelays = []time.Duration{
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
}

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
	Author   string
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
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := prCache.Get(key); ok {
			return cached, nil
		}
	}

	out, err := e.run("pr", "view", strconv.Itoa(number),
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
	pr := PullRequest{
		Number:      raw.Number,
		Title:       raw.Title,
		URL:         raw.URL,
		HeadRefName: raw.HeadRefName,
		Repository:  repo,
		RepoName:    repoName,
		Author:      raw.Author.Login,
		Labels:      labels,
	}
	if runtimeCacheEnabled(e) {
		prCache.Set(key, pr, 2*time.Minute)
	}
	return pr, nil
}

// FetchPRStats returns additions, deletions, and changed file count for a PR.
func FetchPRStats(repo string, number int) (PRStats, error) {
	return fetchPRStatsWith(defaultExecer, repo, number)
}

func fetchPRStatsWith(e execer, repo string, number int) (PRStats, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := prStatsCache.Get(key); ok {
			return cached, nil
		}
	}

	out, err := e.run("pr", "view", strconv.Itoa(number),
		"--repo", repo, "--json", "additions,deletions,changedFiles")
	if err != nil {
		return PRStats{}, fmt.Errorf("gh pr view %d stats: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var s PRStats
	if err := json.Unmarshal(out, &s); err != nil {
		return PRStats{}, fmt.Errorf("parse pr stats: %w", err)
	}
	if runtimeCacheEnabled(e) {
		prStatsCache.Set(key, s, 5*time.Minute)
	}
	return s, nil
}

// FetchPRState fetches the current state of a specific PR by repo and number.
func FetchPRState(repo string, number int) (PRState, error) {
	return fetchPRStateWith(defaultExecer, repo, number)
}

func fetchPRStateWith(e execer, repo string, number int) (PRState, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := prStateCache.Get(key); ok {
			return cached, nil
		}
	}

	out, err := e.run("pr", "view", strconv.Itoa(number),
		"--repo", repo, "--json", "state,mergedAt,mergeable,statusCheckRollup")
	if err != nil {
		return PRState{}, fmt.Errorf("gh pr view %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var s PRState
	if err := json.Unmarshal(out, &s); err != nil {
		return PRState{}, fmt.Errorf("parse pr state: %w", err)
	}
	if runtimeCacheEnabled(e) {
		prStateCache.Set(key, s, 30*time.Second)
	}
	return s, nil
}

// FetchPRFiles returns the paths of files changed by a PR.
func FetchPRFiles(repo string, number int) ([]string, error) {
	return fetchPRFilesWith(defaultExecer, repo, number)
}

func fetchPRFilesWith(e execer, repo string, number int) ([]string, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := prFilesCache.Get(key); ok {
			return append([]string(nil), cached...), nil
		}
	}

	out, err := e.run("pr", "view", strconv.Itoa(number),
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
	if runtimeCacheEnabled(e) {
		prFilesCache.Set(key, append([]string(nil), paths...), 2*time.Minute)
	}
	return paths, nil
}

// FetchPRHeadSHA returns the head commit SHA of a PR. Used to detect a fresh
// push by the PR author after a review was submitted.
func FetchPRHeadSHA(repo string, number int) (string, error) {
	return fetchPRHeadSHAWith(defaultExecer, repo, number)
}

func fetchPRHeadSHAWith(e execer, repo string, number int) (string, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := prHeadSHACache.Get(key); ok {
			return cached, nil
		}
	}

	out, err := e.run("pr", "view", strconv.Itoa(number),
		"--repo", repo, "--json", "headRefOid")
	if err != nil {
		return "", fmt.Errorf("gh pr view %d head: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var raw struct {
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", fmt.Errorf("parse pr head: %w", err)
	}
	if runtimeCacheEnabled(e) {
		prHeadSHACache.Set(key, raw.HeadRefOid, 30*time.Second)
	}
	return raw.HeadRefOid, nil
}

// FetchPRDiff returns the unified diff for a PR. Used by the evaluation
// LLM-as-judge to score the landed change.
func FetchPRDiff(repo string, number int) (string, error) {
	return fetchPRDiffWith(defaultExecer, repo, number)
}

func fetchPRDiffWith(e execer, repo string, number int) (string, error) {
	out, err := e.run("pr", "diff", strconv.Itoa(number), "--repo", repo)
	if err != nil {
		return "", fmt.Errorf("gh pr diff %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// FetchPRBranch returns the head branch name for a PR.
func FetchPRBranch(repo string, number int) (string, error) {
	return fetchPRBranchWith(defaultExecer, repo, number)
}

func fetchPRBranchWith(e execer, repo string, number int) (string, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := prBranchCache.Get(key); ok {
			return cached, nil
		}
	}

	out, err := e.run("pr", "view", strconv.Itoa(number),
		"--repo", repo, "--json", "headRefName")
	if err != nil {
		return "", fmt.Errorf("gh pr view %d branch: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var b PRBranch
	if err := json.Unmarshal(out, &b); err != nil {
		return "", fmt.Errorf("parse pr branch: %w", err)
	}
	if runtimeCacheEnabled(e) {
		prBranchCache.Set(key, b.HeadRefName, 2*time.Minute)
	}
	return b.HeadRefName, nil
}

// FetchPRContext returns the URL, branch, and unresolved review comments for a PR.
func FetchPRContext(repo string, number int) (PRContext, error) {
	return fetchPRContextWith(defaultExecer, repo, number)
}

func fetchPRContextWith(e execer, repo string, number int) (PRContext, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := prContextCache.Get(key); ok {
			return cached, nil
		}
	}

	// Fetch PR metadata: url, branch, and review bodies
	out, err := e.run("pr", "view", strconv.Itoa(number),
		"--repo", repo, "--json", "url,headRefName,author,reviews")
	if err != nil {
		return PRContext{}, fmt.Errorf("gh pr view %d context: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var meta struct {
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
		Reviews []struct {
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

	ctx := PRContext{URL: meta.URL, Branch: meta.HeadRefName, Author: meta.Author.Login}

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
	inlineResp, err := runGHAPIWith(e, "30s",
		fmt.Sprintf("repos/%s/pulls/%d/comments", repo, number),
		"-q", `.[] | select(.position != null) | {author: .user.login, body: .body, path: .path}`)
	if err == nil && len(inlineResp.body) > 0 {
		for line := range strings.SplitSeq(strings.TrimSpace(string(inlineResp.body)), "\n") {
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

	if runtimeCacheEnabled(e) {
		prContextCache.Set(key, ctx, 30*time.Second)
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
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := prClosingIssuesCache.Get(key); ok {
			return append([]int(nil), cached.issues...), cached.body, nil
		}
	}

	out, runErr := e.run("pr", "view", strconv.Itoa(number),
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
	if runtimeCacheEnabled(e) {
		prClosingIssuesCache.Set(key, prClosingIssuesResult{
			issues: append([]int(nil), issues...),
			body:   raw.Body,
		}, 2*time.Minute)
	}
	return issues, raw.Body, nil
}

// MergePR merges a pull request using squash strategy.
func MergePR(repo string, number int) error {
	return mergePRWith(defaultExecer, repo, number)
}

func mergePRWith(e execer, repo string, number int) error {
	var lastOut []byte
	var lastErr error
	for attempt := 0; attempt <= len(mergeRetryDelays); attempt++ {
		out, err := e.run("pr", "merge", strconv.Itoa(number),
			"--repo", repo, "--squash")
		if err == nil {
			if runtimeCacheEnabled(e) {
				invalidatePRCaches(repo, number)
			}
			return nil
		}
		lastOut, lastErr = out, err
		if !isBaseBranchModifiedErr(out) || attempt == len(mergeRetryDelays) {
			break
		}
		time.Sleep(mergeRetryDelays[attempt])
	}
	return fmt.Errorf("gh pr merge %d: %s: %w", number, strings.TrimSpace(string(lastOut)), lastErr)
}

// isBaseBranchModifiedErr reports whether gh's output indicates a transient
// base-branch race that GitHub asks the caller to retry.
func isBaseBranchModifiedErr(out []byte) bool {
	return strings.Contains(string(out), "Base branch was modified")
}

// MarkReady marks a draft pull request as ready for review.
func MarkReady(repo string, number int) error {
	return markReadyWith(defaultExecer, repo, number)
}

func markReadyWith(e execer, repo string, number int) error {
	out, err := e.run("pr", "ready", strconv.Itoa(number), "-R", repo)
	if err != nil {
		return fmt.Errorf("gh pr ready: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if runtimeCacheEnabled(e) {
		invalidatePRCaches(repo, number)
	}
	return nil
}

// EditPRBody replaces the body of a pull request.
func EditPRBody(repo string, number int, body string) error {
	return editPRBodyWith(defaultExecer, repo, number, body)
}

func editPRBodyWith(e execer, repo string, number int, body string) error {
	out, err := e.run("pr", "edit", strconv.Itoa(number),
		"--repo", repo, "--body", body)
	if err != nil {
		return fmt.Errorf("gh pr edit %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	if runtimeCacheEnabled(e) {
		invalidatePRCaches(repo, number)
	}
	return nil
}

// RequestReviewers requests a review from the given GitHub user logins.
func RequestReviewers(repo string, number int, reviewers []string) error {
	return requestReviewersWith(defaultExecer, repo, number, reviewers)
}

func requestReviewersWith(e execer, repo string, number int, reviewers []string) error {
	if len(reviewers) == 0 {
		return nil
	}
	args := []string{
		"--method", "POST",
		fmt.Sprintf("repos/%s/pulls/%d/requested_reviewers", repo, number),
	}
	for _, reviewer := range reviewers {
		if strings.TrimSpace(reviewer) == "" {
			continue
		}
		args = append(args, "-f", "reviewers[]="+reviewer)
	}
	if len(args) == 3 {
		return nil
	}
	resp, err := runGHAPIWith(e, "", args...)
	if err != nil {
		return fmt.Errorf("gh request reviewers %d: %s: %w", number, sanitizeGHOutput(resp.body), err)
	}
	if runtimeCacheEnabled(e) {
		invalidatePRCaches(repo, number)
	}
	return nil
}
