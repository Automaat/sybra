package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
	// BaseRefName is the PR's target branch.
	BaseRefName string `json:"baseRefName"`
	// AutoMergeEnabled reports whether GitHub's native auto-merge is armed,
	// derived from `autoMergeRequest` being non-null.
	AutoMergeEnabled bool `json:"-"`
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

// Resolved reports whether this PR needs no further pr-fix action: it
// merged, or it's open with green CI and no merge conflicts. An abandoned
// (unmerged) close is NOT resolved — unlike a stale-worktree-vs-green-remote
// false positive, a human genuinely needs to decide what happens to the task
// now. Callers use this to re-probe the live PR before parking a task
// human-required on a stale/diverged local worktree — an external bot may
// have force-pushed a fix since the last poll, leaving nothing left to do.
func (s PRState) Resolved() bool {
	return s.State == "MERGED" || s.ReadyToMerge()
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
		"--repo", repo, "--json", "state,mergedAt,mergeable,statusCheckRollup,baseRefName,autoMergeRequest")
	if err != nil {
		return PRState{}, fmt.Errorf("gh pr view %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var raw struct {
		PRState
		AutoMergeRequest *struct {
			EnabledAt string `json:"enabledAt"`
		} `json:"autoMergeRequest"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return PRState{}, fmt.Errorf("parse pr state: %w", err)
	}
	s := raw.PRState
	s.AutoMergeEnabled = raw.AutoMergeRequest != nil
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

// FetchPRBaseSHAContext is a context-bounded lookup for the current base commit
// SHA of a PR.
func FetchPRBaseSHAContext(ctx context.Context, repo string, number int) (string, error) {
	key := prCacheKey(repo, number)
	if cached, ok := prBaseSHACache.Get(key); ok {
		return cached, nil
	}
	out, err := ghRunCtx(ctx, "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", "baseRefOid")
	if err != nil {
		return "", fmt.Errorf("gh pr view %d base: %s: %w", number, sanitizeGHOutput(out), err)
	}
	var raw struct {
		BaseRefOid string `json:"baseRefOid"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", fmt.Errorf("parse pr base: %w", err)
	}
	prBaseSHACache.Set(key, raw.BaseRefOid, 30*time.Second)
	return raw.BaseRefOid, nil
}

// FetchCommitParentSHAs returns a commit's parent SHAs in Git's parent order.
func FetchCommitParentSHAs(repo, sha string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return FetchCommitParentSHAsContext(ctx, repo, sha)
}

// FetchCommitParentSHAsContext is a context-bounded FetchCommitParentSHAs.
func FetchCommitParentSHAsContext(ctx context.Context, repo, sha string) ([]string, error) {
	return fetchCommitParentSHAsWith(ctx, defaultExecer, repo, sha)
}

func fetchCommitParentSHAsWith(ctx context.Context, e execer, repo, sha string) ([]string, error) {
	if strings.TrimSpace(sha) == "" {
		return nil, fmt.Errorf("fetch commit parents for %s: empty sha", repo)
	}
	if !isCommitSHA(sha) {
		return nil, fmt.Errorf("fetch commit parents for %s: invalid commit sha %q", repo, sha)
	}
	key := repo + "@" + sha
	if runtimeCacheEnabled(e) {
		if cached, ok := commitParentsCache.Get(key); ok {
			return append([]string(nil), cached...), nil
		}
	}

	resp, err := runGHAPICtxWith(ctx, e, "30s", fmt.Sprintf("repos/%s/commits/%s", repo, sha), "--jq", ".parents[].sha")
	if err != nil {
		return nil, fmt.Errorf("gh api commit %s: %s: %w", sha, sanitizeGHOutput(resp.body), err)
	}
	parents := parseCommitParentSHAs(resp.body)
	if runtimeCacheEnabled(e) {
		commitParentsCache.Set(key, append([]string(nil), parents...), 5*time.Minute)
	}
	return parents, nil
}

// FetchBaseOnlyMergeFromReviewed reports whether head is a two-parent merge
// commit whose first parent is the reviewed commit and whose second parent is
// reachable from the PR base branch. If any GitHub lookup fails the caller
// should treat the lineage as unknown, not as author advancement.
func FetchBaseOnlyMergeFromReviewed(ctx context.Context, repo string, number int, headSHA, reviewedSHA string) (bool, error) {
	parents, err := FetchCommitParentSHAsContext(ctx, repo, headSHA)
	if err != nil {
		return false, err
	}
	if len(parents) != 2 || parents[0] != reviewedSHA {
		return false, nil
	}
	baseSHA, err := FetchPRBaseSHAContext(ctx, repo, number)
	if err != nil {
		return false, err
	}
	cmp, err := FetchPRCompare(ctx, repo, parents[1], baseSHA)
	if err != nil {
		return false, err
	}
	return isBaseOnlyMergeFromReviewed(parents, reviewedSHA, cmp.Status), nil
}

func isCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func isBaseOnlyMergeFromReviewed(parents []string, reviewedSHA, baseCompareStatus string) bool {
	if len(parents) != 2 || parents[0] != reviewedSHA {
		return false
	}
	return baseCompareStatus == "identical" || baseCompareStatus == "ahead"
}

func parseCommitParentSHAs(out []byte) []string {
	lines := strings.Split(string(out), "\n")
	parents := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			parents = append(parents, line)
		}
	}
	return parents
}

// PRCompare summarizes the difference between two commits (base...head).
// Commits is how many commits head is ahead of base; Additions/Deletions sum the
// line churn. Note: GitHub caps the compare files list at 300, so on very large
// edits the line counts undercount (Commits stays accurate).
type PRCompare struct {
	Commits   int    `json:"total_commits"`
	Status    string `json:"-"` // ahead | behind | diverged | identical
	Additions int    `json:"-"`
	Deletions int    `json:"-"`
}

// FetchPRCompare returns how far head is ahead of base (status, commits, line
// churn). Used at landing to measure human edits made after the agent's last
// push. Context-bounded so it can't stall the poll loop.
func FetchPRCompare(ctx context.Context, repo, base, head string) (PRCompare, error) {
	out, err := ghRunCtx(ctx, "api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, base, head))
	if err != nil {
		return PRCompare{}, fmt.Errorf("gh api compare %s...%s: %s: %w", base, head, sanitizeGHOutput(out), err)
	}
	return parsePRCompare(out)
}

func parsePRCompare(out []byte) (PRCompare, error) {
	var raw struct {
		TotalCommits int    `json:"total_commits"`
		Status       string `json:"status"`
		Files        []struct {
			Additions int `json:"additions"`
			Deletions int `json:"deletions"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return PRCompare{}, fmt.Errorf("parse compare: %w", err)
	}
	c := PRCompare{Commits: raw.TotalCommits, Status: raw.Status}
	for _, f := range raw.Files {
		c.Additions += f.Additions
		c.Deletions += f.Deletions
	}
	return c, nil
}

// FetchPRStatsContext is a context-bounded FetchPRStats for the poll path.
func FetchPRStatsContext(ctx context.Context, repo string, number int) (PRStats, error) {
	out, err := ghRunCtx(ctx, "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", "additions,deletions,changedFiles")
	if err != nil {
		return PRStats{}, fmt.Errorf("gh pr view %d stats: %s: %w", number, sanitizeGHOutput(out), err)
	}
	var s PRStats
	if err := json.Unmarshal(out, &s); err != nil {
		return PRStats{}, fmt.Errorf("parse pr stats: %w", err)
	}
	return s, nil
}

// FetchPRHeadState returns the head commit SHA, open/closed state, and
// updatedAt timestamp of a PR. Unlike FetchPRHeadSHA, it also detects a PR
// that was merged or closed at the same head commit it had while open — a
// head-SHA-only probe cannot tell those apart, since merging or closing a PR
// does not change its head SHA. updatedAt further detects a new review or
// status/check event landing at the same head commit (e.g. a re-run CI
// job failing, or a reviewer requesting changes) — GitHub bumps a PR's
// updatedAt on any such event even without a new commit.
// Deliberately uncached: callers use this to validate a cached ready-PR
// snapshot, and caching it would reintroduce the same staleness it exists to
// catch.
// Context-bounded (10s) so a stalled probe in the poll path releases the
// global ghGate instead of holding it for the kernel TCP timeout.
func FetchPRHeadState(repo string, number int) (sha string, open bool, updatedAt string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return fetchPRHeadStateWith(ctx, defaultExecer, repo, number)
}

func fetchPRHeadStateWith(ctx context.Context, e execer, repo string, number int) (sha string, open bool, updatedAt string, err error) {
	out, err := runE(ctx, e, "pr", "view", strconv.Itoa(number),
		"--repo", repo, "--json", "headRefOid,state,updatedAt")
	if err != nil {
		return "", false, "", fmt.Errorf("gh pr view %d head state: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var raw struct {
		HeadRefOid string `json:"headRefOid"`
		State      string `json:"state"`
		UpdatedAt  string `json:"updatedAt"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", false, "", fmt.Errorf("parse pr head state: %w", err)
	}
	return raw.HeadRefOid, raw.State == "OPEN", raw.UpdatedAt, nil
}

// FetchPRHeadSHAContext is a context-bounded FetchPRHeadSHA for the poll path.
func FetchPRHeadSHAContext(ctx context.Context, repo string, number int) (string, error) {
	out, err := ghRunCtx(ctx, "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", "headRefOid")
	if err != nil {
		return "", fmt.Errorf("gh pr view %d head: %s: %w", number, sanitizeGHOutput(out), err)
	}
	var raw struct {
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", fmt.Errorf("parse pr head: %w", err)
	}
	return raw.HeadRefOid, nil
}

// FetchPRMergeCommitContext returns the default-branch commit SHA a merged PR
// produced (empty if not merged). Context-bounded for the poll path.
func FetchPRMergeCommitContext(ctx context.Context, repo string, number int) (string, error) {
	out, err := ghRunCtx(ctx, "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", "mergeCommit")
	if err != nil {
		return "", fmt.Errorf("gh pr view %d mergeCommit: %s: %w", number, sanitizeGHOutput(out), err)
	}
	var raw struct {
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", fmt.Errorf("parse merge commit: %w", err)
	}
	return raw.MergeCommit.OID, nil
}

// FetchRecentCommitMessages returns the messages of the most recent commits on
// the repo's default branch (up to limit, capped at 100). Used by revert
// detection to spot a "This reverts commit <sha>" referencing a landed merge
// commit. Context-bounded.
func FetchRecentCommitMessages(ctx context.Context, repo string, limit int) ([]string, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	out, err := ghRunCtx(ctx, "api",
		fmt.Sprintf("repos/%s/commits?per_page=%d", repo, limit),
		"--jq", ".[].commit.message")
	if err != nil {
		return nil, fmt.Errorf("gh api commits %s: %s: %w", repo, sanitizeGHOutput(out), err)
	}
	return parseCommitMessages(out), nil
}

// parseCommitMessages splits the newline-delimited messages from the --jq
// extraction, dropping blanks.
func parseCommitMessages(out []byte) []string {
	lines := strings.Split(string(out), "\n")
	msgs := make([]string, 0, len(lines))
	for _, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			msgs = append(msgs, s)
		}
	}
	return msgs
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

// EnableAutoMerge arms GitHub's native auto-merge (squash) on a PR. Native
// auto-merge then waits for the base branch's required checks to go green
// and merges on GitHub's own infrastructure — an accelerator on top of the
// green-gated MergePR path, not a replacement for it.
func EnableAutoMerge(repo string, number int) error {
	return enableAutoMergeWith(defaultExecer, repo, number)
}

func enableAutoMergeWith(e execer, repo string, number int) error {
	out, err := e.run("pr", "merge", strconv.Itoa(number),
		"--repo", repo, "--auto", "--squash")
	if err != nil {
		return fmt.Errorf("gh pr merge --auto %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	if runtimeCacheEnabled(e) {
		invalidatePRCaches(repo, number)
	}
	return nil
}

// ghRepoAutoMergeSetting is the subset of the repo settings payload
// SupportsNativeAutoMerge needs.
type ghRepoAutoMergeSetting struct {
	AllowAutoMerge bool `json:"allow_auto_merge"`
}

// ghBranchProtection is the subset of the branch protection payload
// SupportsNativeAutoMerge needs. Required status checks can surface as
// either the legacy `contexts` array or the newer `checks` array depending
// on the gh/API version, so both are checked.
type ghBranchProtection struct {
	RequiredStatusChecks *struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	} `json:"required_status_checks"`
	RequiredConversationResolution *struct {
		Enabled bool `json:"enabled"`
	} `json:"required_conversation_resolution"`
}

// nativeAutoMergeCache short-TTL-caches the base-branch capability check,
// keyed by repo+"@"+baseBranch.
var nativeAutoMergeCache = newTTLCache[bool]()

// SupportsNativeAutoMerge reports whether repo allows native auto-merge and
// baseBranch's protection requires both status checks and conversation
// resolution — the minimum GitHub needs to make native auto-merge behave
// like Sybra's own green-gated merge. baseBranch must be the PR's base
// (target) branch, never its head branch.
//
// Fails CLOSED: any missing prerequisite, or any gh API error (including a
// 404 from a repo with no branch protection configured, a common and valid
// state), returns (false, nil) rather than an error. A non-nil error is
// reserved for something unexpected — e.g. malformed JSON in an otherwise
// successful response.
func SupportsNativeAutoMerge(repo, baseBranch string) (bool, error) {
	return supportsNativeAutoMergeWith(defaultExecer, repo, baseBranch)
}

func supportsNativeAutoMergeWith(e execer, repo, baseBranch string) (bool, error) {
	if repo == "" || baseBranch == "" {
		return false, nil
	}
	key := repo + "@" + baseBranch
	if runtimeCacheEnabled(e) {
		if cached, ok := nativeAutoMergeCache.Get(key); ok {
			return cached, nil
		}
	}

	// A gh API error on either lookup below (most commonly a 404 from a repo
	// with no branch protection configured) fails CLOSED: the repo/branch
	// simply doesn't support native auto-merge, which is an unsupported —
	// not exceptional — state, so it is folded into the boolean result rather
	// than propagated as an error.
	repoResp, repoAPIErr := runGHAPIWith(e, "3m", fmt.Sprintf("repos/%s", repo))
	allowAutoMerge := false
	if repoAPIErr == nil {
		var repoSettings ghRepoAutoMergeSetting
		if jsonErr := json.Unmarshal(repoResp.body, &repoSettings); jsonErr != nil {
			return false, fmt.Errorf("parse repo settings %s: %w", repo, jsonErr)
		}
		allowAutoMerge = repoSettings.AllowAutoMerge
	}
	if !allowAutoMerge {
		return setNativeAutoMergeCache(e, key, false), nil
	}

	protResp, protAPIErr := runGHAPIWith(e, "3m", fmt.Sprintf("repos/%s/branches/%s/protection", repo, url.PathEscape(baseBranch)))
	supported := false
	if protAPIErr == nil {
		var prot ghBranchProtection
		if jsonErr := json.Unmarshal(protResp.body, &prot); jsonErr != nil {
			return false, fmt.Errorf("parse branch protection %s@%s: %w", repo, baseBranch, jsonErr)
		}
		hasRequiredChecks := prot.RequiredStatusChecks != nil &&
			(len(prot.RequiredStatusChecks.Contexts) > 0 || len(prot.RequiredStatusChecks.Checks) > 0)
		requiresConversationResolution := prot.RequiredConversationResolution != nil &&
			prot.RequiredConversationResolution.Enabled
		supported = hasRequiredChecks && requiresConversationResolution
	}
	return setNativeAutoMergeCache(e, key, supported), nil
}

func setNativeAutoMergeCache(e execer, key string, val bool) bool {
	if runtimeCacheEnabled(e) {
		nativeAutoMergeCache.Set(key, val, 3*time.Minute)
	}
	return val
}

// MergePRViaREST merges a pull request over GitHub's REST API (PUT
// .../merge), passing the observed head SHA as the sha parameter so GitHub
// aborts the merge (409) instead of squashing on stale evidence if the head
// moved since it was fetched. Used by the REST-sourced auto-merge path so the
// whole merge stays on the idle REST bucket instead of GraphQL's `gh pr
// merge`. A head-SHA mismatch is terminal — never retried; only the transient
// base-branch race is retried, mirroring mergePRWith.
func MergePRViaREST(repo string, number int, headSHA string) error {
	return mergePRViaRESTWith(defaultExecer, repo, number, headSHA)
}

func mergePRViaRESTWith(e execer, repo string, number int, headSHA string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || number <= 0 {
		return fmt.Errorf("invalid repo or PR: %s#%d", repo, number)
	}
	if headSHA == "" {
		return fmt.Errorf("missing head SHA for %s#%d: refusing unprotected merge", repo, number)
	}
	var lastResp ghHTTPResponse
	var lastErr error
	for attempt := 0; attempt <= len(mergeRetryDelays); attempt++ {
		resp, err := runGHAPIWith(e, "",
			fmt.Sprintf("repos/%s/%s/pulls/%d/merge", owner, name, number),
			"--method", "PUT",
			"-f", "merge_method=squash",
			"-f", "sha="+headSHA,
		)
		if err == nil {
			if runtimeCacheEnabled(e) {
				invalidatePRCaches(repo, number)
			}
			return nil
		}
		lastResp, lastErr = resp, err
		if isHeadSHAMismatchErr(resp) || !isBaseBranchModifiedErr(resp.body) || attempt == len(mergeRetryDelays) {
			break
		}
		time.Sleep(mergeRetryDelays[attempt])
	}
	return fmt.Errorf("gh api merge %d: %s: %w", number, sanitizeGHOutput(lastResp.body), lastErr)
}

// isHeadSHAMismatchErr reports whether the REST merge response indicates the
// supplied sha no longer matches the PR's current head — GitHub returns 409
// with this specific message. Terminal: retrying would only re-merge on stale
// evidence once the head advances further, so the caller must not retry it.
// Deliberately keyed on the message rather than the bare 409 status, since
// the merge endpoint also returns 409 for other conditions (e.g. the
// transient base-branch race handled by isBaseBranchModifiedErr), which must
// stay retryable.
func isHeadSHAMismatchErr(resp ghHTTPResponse) bool {
	return strings.Contains(string(resp.body), "Head branch was modified")
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
