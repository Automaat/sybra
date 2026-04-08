package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// execer abstracts command execution for testing.
type execer interface {
	run(args ...string) ([]byte, error)
}

type ghExecer struct{}

func (ghExecer) run(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	return cmd.CombinedOutput()
}

var defaultExecer execer = ghExecer{}

var cachedViewer string

func viewerLogin(e execer) string {
	if cachedViewer != "" {
		return cachedViewer
	}
	out, err := e.run("api", "user", "-q", ".login")
	if err != nil {
		return ""
	}
	cachedViewer = strings.TrimSpace(string(out))
	return cachedViewer
}

const prQuery = `query($q: String!) {
  search(query: $q, type: ISSUE, first: 50) {
    nodes {
      ... on PullRequest {
        number
        title
        url
        headRefName
        isDraft
        mergeable
        createdAt
        updatedAt
        reviewDecision
        author { login type: __typename }
        repository { name nameWithOwner }
        labels(first: 10) { nodes { name } }
        commits(last: 1) {
          nodes {
            commit {
              statusCheckRollup { state }
            }
          }
        }
        reviewThreads(first: 100) {
          nodes { isResolved }
        }
        latestReviews(first: 20) {
          nodes { state author { login } }
        }
      }
    }
  }
}`

type gqlResponse struct {
	Data struct {
		Search struct {
			Nodes []gqlPR `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type gqlPR struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	HeadRefName    string `json:"headRefName"`
	IsDraft        bool   `json:"isDraft"`
	Mergeable      string `json:"mergeable"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	ReviewDecision string `json:"reviewDecision"`
	Author         struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"author"`
	Repository struct {
		Name          string `json:"name"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	ReviewThreads struct {
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	LatestReviews struct {
		Nodes []struct {
			State  string `json:"state"`
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"latestReviews"`
}

// FetchReviews returns open PRs created by the user and review requests, excluding bots.
func FetchReviews() (ReviewSummary, error) {
	return fetchReviewsWith(defaultExecer)
}

func fetchReviewsWith(e execer) (ReviewSummary, error) {
	var summary ReviewSummary

	created, err := searchPRsWith(e, "is:pr is:open author:@me")
	if err != nil {
		return summary, fmt.Errorf("fetch created PRs: %w", err)
	}
	summary.CreatedByMe = created

	requested, err := searchPRsWith(e, "is:pr is:open review-requested:@me")
	if err != nil {
		return summary, fmt.Errorf("fetch review requests: %w", err)
	}
	summary.ReviewRequested = requested

	return summary, nil
}

func searchPRsWith(e execer, query string) ([]PullRequest, error) {
	out, err := e.run("api", "graphql",
		"-f", "query="+prQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var resp gqlResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}

	return convertPRs(resp.Data.Search.Nodes, viewerLogin(e)), nil
}

func convertPRs(nodes []gqlPR, viewer string) []PullRequest {
	prs := make([]PullRequest, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if isBot(n.Author.Type, n.Author.Login) {
			continue
		}

		labels := make([]string, 0, len(n.Labels.Nodes))
		for _, l := range n.Labels.Nodes {
			labels = append(labels, l.Name)
		}

		var ciStatus string
		if len(n.Commits.Nodes) > 0 {
			if rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
				ciStatus = rollup.State
			}
		}

		var unresolved int
		for _, t := range n.ReviewThreads.Nodes {
			if !t.IsResolved {
				unresolved++
			}
		}

		var viewerApproved bool
		if viewer != "" {
			for _, r := range n.LatestReviews.Nodes {
				if strings.EqualFold(r.Author.Login, viewer) && r.State == "APPROVED" {
					viewerApproved = true
					break
				}
			}
		}

		prs = append(prs, PullRequest{
			Number:            n.Number,
			Title:             n.Title,
			URL:               n.URL,
			HeadRefName:       n.HeadRefName,
			Repository:        n.Repository.NameWithOwner,
			RepoName:          n.Repository.Name,
			Author:            n.Author.Login,
			IsDraft:           n.IsDraft,
			Mergeable:         n.Mergeable,
			Labels:            labels,
			CIStatus:          ciStatus,
			ReviewDecision:    n.ReviewDecision,
			UnresolvedCount:   unresolved,
			ViewerHasApproved: viewerApproved,
			CreatedAt:         n.CreatedAt,
			UpdatedAt:         n.UpdatedAt,
		})
	}
	return prs
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

func isBot(typeName, login string) bool {
	return typeName == "Bot" || strings.Contains(login, "[bot]")
}

// renovatePRQuery includes individual check run contexts for rerun support.
const renovatePRQuery = `query($q: String!) {
  search(query: $q, type: ISSUE, first: 50) {
    nodes {
      ... on PullRequest {
        number
        title
        url
        headRefName
        isDraft
        mergeable
        createdAt
        updatedAt
        reviewDecision
        author { login type: __typename }
        repository { name nameWithOwner }
        labels(first: 10) { nodes { name } }
        commits(last: 1) {
          nodes {
            commit {
              statusCheckRollup {
                state
                contexts(first: 50) {
                  nodes {
                    ... on CheckRun {
                      name
                      status
                      conclusion
                    }
                  }
                }
              }
            }
          }
        }
        reviewThreads(first: 100) {
          nodes { isResolved }
        }
        latestReviews(first: 20) {
          nodes { state author { login } }
        }
      }
    }
  }
}`

type gqlRenovateResponse struct {
	Data struct {
		Search struct {
			Nodes []gqlRenovatePR `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type gqlRenovatePR struct {
	gqlPR
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Name       string `json:"name"`
							Status     string `json:"status"`
							Conclusion string `json:"conclusion"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// FetchRenovatePRs returns Renovate bot PRs for the given repositories.
func FetchRenovatePRs(author string, repos []string) ([]RenovatePR, error) {
	return fetchRenovatePRsWith(defaultExecer, author, repos)
}

func fetchRenovatePRsWith(e execer, author string, repos []string) ([]RenovatePR, error) {
	if len(repos) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var all []RenovatePR

	authors := []string{author}
	if author == "app/renovate" {
		authors = append(authors, "renovate[bot]")
	}

	for _, repo := range repos {
		for _, a := range authors {
			query := fmt.Sprintf("is:pr is:open author:%s repo:%s", a, repo)
			prs, err := searchRenovatePRsWith(e, query)
			if err != nil {
				return nil, fmt.Errorf("fetch renovate PRs for %s: %w", repo, err)
			}
			for i := range prs {
				key := fmt.Sprintf("%s#%d", prs[i].Repository, prs[i].Number)
				if _, dup := seen[key]; !dup {
					seen[key] = struct{}{}
					all = append(all, prs[i])
				}
			}
		}
	}
	return all, nil
}

func searchRenovatePRsWith(e execer, query string) ([]RenovatePR, error) {
	out, err := e.run("api", "graphql",
		"-f", "query="+renovatePRQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var resp gqlRenovateResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}

	return convertRenovatePRs(resp.Data.Search.Nodes, viewerLogin(e)), nil
}

func convertRenovatePRs(nodes []gqlRenovatePR, viewer string) []RenovatePR {
	prs := make([]RenovatePR, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]

		labels := make([]string, 0, len(n.Labels.Nodes))
		for _, l := range n.Labels.Nodes {
			labels = append(labels, l.Name)
		}

		var ciStatus string
		var checks []CheckRunInfo
		if len(n.Commits.Nodes) > 0 {
			if rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
				ciStatus = rollup.State
				for _, ctx := range rollup.Contexts.Nodes {
					if ctx.Name == "" {
						continue
					}
					checks = append(checks, CheckRunInfo{
						Name:       ctx.Name,
						Status:     ctx.Status,
						Conclusion: ctx.Conclusion,
					})
				}
			}
		}

		var unresolved int
		for _, t := range n.ReviewThreads.Nodes {
			if !t.IsResolved {
				unresolved++
			}
		}

		var viewerApproved bool
		if viewer != "" {
			for _, r := range n.LatestReviews.Nodes {
				if strings.EqualFold(r.Author.Login, viewer) && r.State == "APPROVED" {
					viewerApproved = true
					break
				}
			}
		}

		prs = append(prs, RenovatePR{
			PullRequest: PullRequest{
				Number:            n.Number,
				Title:             n.Title,
				URL:               n.URL,
				HeadRefName:       n.HeadRefName,
				Repository:        n.Repository.NameWithOwner,
				RepoName:          n.Repository.Name,
				Author:            n.Author.Login,
				IsDraft:           n.IsDraft,
				Mergeable:         n.Mergeable,
				Labels:            labels,
				CIStatus:          ciStatus,
				ReviewDecision:    n.ReviewDecision,
				UnresolvedCount:   unresolved,
				ViewerHasApproved: viewerApproved,
				CreatedAt:         n.CreatedAt,
				UpdatedAt:         n.UpdatedAt,
			},
			CheckRuns: checks,
		})
	}
	return prs
}

// ApprovePR approves a pull request.
func ApprovePR(repo string, number int) error {
	return approvePRWith(defaultExecer, repo, number)
}

func approvePRWith(e execer, repo string, number int) error {
	out, err := e.run("pr", "review", "--approve",
		fmt.Sprintf("%d", number), "-R", repo)
	if err != nil {
		return fmt.Errorf("gh pr review --approve %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RerunFailedChecks reruns the latest failed workflow run for a PR.
func RerunFailedChecks(repo string, number int) error {
	return rerunFailedChecksWith(defaultExecer, repo, number)
}

func rerunFailedChecksWith(e execer, repo string, number int) error {
	// Get the PR branch to find the latest run
	branch, err := fetchPRBranchWith(e, repo, number)
	if err != nil {
		return err
	}
	out, err := e.run("run", "rerun", "--failed",
		"--repo", repo, "--branch", branch)
	if err != nil {
		return fmt.Errorf("gh run rerun --failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// HasPendingReview checks if the authenticated user has a pending (draft) review on a PR.
// Pending reviews are only visible to their author via the REST API.
func HasPendingReview(repo string, number int) (bool, error) {
	return hasPendingReviewWith(defaultExecer, repo, number)
}

func hasPendingReviewWith(e execer, repo string, number int) (bool, error) {
	out, err := e.run("api", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number))
	if err != nil {
		return false, fmt.Errorf("fetch reviews for %s#%d: %s: %w", repo, number, strings.TrimSpace(string(out)), err)
	}
	var reviews []struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &reviews); err != nil {
		return false, fmt.Errorf("parse reviews: %w", err)
	}
	for i := range reviews {
		if reviews[i].State == "PENDING" {
			return true, nil
		}
	}
	return false, nil
}

// PRStats holds size metrics for a pull request.
type PRStats struct {
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changedFiles"`
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

// PRFiles holds the list of files changed by a PR.
type PRFiles struct {
	Files []struct {
		Path string `json:"path"`
	} `json:"files"`
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

// PRBranch holds the head branch name of a PR.
type PRBranch struct {
	HeadRefName string `json:"headRefName"`
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

const issueQuery = `query($q: String!) {
  search(query: $q, type: ISSUE, first: 50) {
    nodes {
      ... on Issue {
        number
        title
        body
        url
        state
        createdAt
        updatedAt
        author { login }
        repository { name nameWithOwner }
        labels(first: 10) { nodes { name } }
      }
    }
  }
}`

type gqlIssueResponse struct {
	Data struct {
		Search struct {
			Nodes []gqlIssue `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type gqlIssue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository struct {
		Name          string `json:"name"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

// FetchAssignedIssues returns open issues assigned to the authenticated user.
func FetchAssignedIssues() ([]Issue, error) {
	return fetchAssignedIssuesWith(defaultExecer)
}

func fetchAssignedIssuesWith(e execer) ([]Issue, error) {
	return searchIssuesWith(e, "is:issue is:open assignee:@me sort:updated-desc")
}

// FetchLabeledIssuesForRepos returns open issues with the given label across the specified repos.
func FetchLabeledIssuesForRepos(repos []string, label string) ([]Issue, error) {
	return fetchLabeledIssuesForReposWith(defaultExecer, repos, label)
}

func fetchLabeledIssuesForReposWith(e execer, repos []string, label string) ([]Issue, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	parts := make([]string, len(repos))
	for i, r := range repos {
		parts[i] = "repo:" + r
	}
	query := fmt.Sprintf("is:issue is:open label:%s %s sort:updated-desc", label, strings.Join(parts, " "))
	return searchIssuesWith(e, query)
}

func searchIssuesWith(e execer, query string) ([]Issue, error) {
	out, err := e.run("api", "graphql",
		"-f", "query="+issueQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", strings.TrimSpace(string(out)), err)
	}

	var resp gqlIssueResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}

	return convertIssues(resp.Data.Search.Nodes), nil
}

func convertIssues(nodes []gqlIssue) []Issue {
	issues := make([]Issue, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		// Skip PRs that sneak in (they are technically issues).
		if n.URL == "" || n.Number == 0 {
			continue
		}

		labels := make([]string, 0, len(n.Labels.Nodes))
		for _, l := range n.Labels.Nodes {
			labels = append(labels, l.Name)
		}

		issues = append(issues, Issue{
			Number:     n.Number,
			Title:      n.Title,
			Body:       n.Body,
			URL:        n.URL,
			Repository: n.Repository.NameWithOwner,
			RepoName:   n.Repository.Name,
			Labels:     labels,
			Author:     n.Author.Login,
			CreatedAt:  n.CreatedAt,
			UpdatedAt:  n.UpdatedAt,
		})
	}
	return issues
}

// ParseIssueURL extracts owner/repo and issue number from a GitHub issue URL.
// Returns ("", 0) if the URL doesn't match.
func ParseIssueURL(rawURL string) (repo string, number int) {
	// https://github.com/owner/repo/issues/123
	if !strings.HasPrefix(rawURL, "https://github.com/") {
		return "", 0
	}
	parts := strings.Split(strings.TrimPrefix(rawURL, "https://github.com/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return "", 0
	}
	n := 0
	for _, c := range parts[3] {
		if c < '0' || c > '9' {
			return "", 0
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return "", 0
	}
	return parts[0] + "/" + parts[1], n
}

// FetchIssue fetches a single issue by repo (owner/repo) and number.
func FetchIssue(repo string, number int) (Issue, error) {
	return fetchIssueWith(defaultExecer, repo, number)
}

func fetchIssueWith(e execer, repo string, number int) (Issue, error) {
	out, err := e.run("issue", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "number,title,body,url,labels,author")
	if err != nil {
		return Issue{}, fmt.Errorf("gh issue view %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		URL    string `json:"url"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Issue{}, fmt.Errorf("parse issue: %w", err)
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
	return Issue{
		Number:     raw.Number,
		Title:      raw.Title,
		Body:       raw.Body,
		URL:        raw.URL,
		Repository: repo,
		RepoName:   repoName,
		Labels:     labels,
		Author:     raw.Author.Login,
	}, nil
}
