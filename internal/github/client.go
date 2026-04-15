package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

var (
	viewerMu     sync.RWMutex
	cachedViewer string
)

func viewerLogin(e execer) string {
	viewerMu.RLock()
	cached := cachedViewer
	viewerMu.RUnlock()
	if cached != "" {
		return cached
	}
	out, err := e.run("api", "user", "-q", ".login")
	if err != nil {
		return ""
	}
	viewerMu.Lock()
	// Double-checked: another goroutine may have populated the cache
	// between RUnlock and Lock; keep whichever value is set.
	if cachedViewer == "" {
		cachedViewer = strings.TrimSpace(string(out))
	}
	result := cachedViewer
	viewerMu.Unlock()
	return result
}

// resetCachedViewerForTest clears the package-level viewer cache. Exported
// only for use from tests in the same package.
func resetCachedViewerForTest() {
	viewerMu.Lock()
	cachedViewer = ""
	viewerMu.Unlock()
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
              statusCheckRollup {
                state
                contexts(first: 50) {
                  nodes {
                    ... on CheckRun { status }
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

type gqlCheckContext struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type gqlStatusCheckRollup struct {
	State    string `json:"state"`
	Contexts struct {
		Nodes []gqlCheckContext `json:"nodes"`
	} `json:"contexts"`
}

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
				StatusCheckRollup *gqlStatusCheckRollup `json:"statusCheckRollup"`
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

// convertCommonPR converts shared gqlPR fields into a PullRequest.
// It does not apply any bot filtering; callers decide whether to filter.
func convertCommonPR(n *gqlPR, viewer string) PullRequest {
	labels := make([]string, 0, len(n.Labels.Nodes))
	for _, l := range n.Labels.Nodes {
		labels = append(labels, l.Name)
	}

	var ciStatus string
	var hasPendingChecks bool
	if len(n.Commits.Nodes) > 0 {
		if rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
			ciStatus = rollup.State
			for _, ctx := range rollup.Contexts.Nodes {
				if ctx.Status != "" && ctx.Status != "COMPLETED" {
					hasPendingChecks = true
					break
				}
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

	return PullRequest{
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
		HasPendingChecks:  hasPendingChecks,
		ReviewDecision:    n.ReviewDecision,
		UnresolvedCount:   unresolved,
		ViewerHasApproved: viewerApproved,
		CreatedAt:         n.CreatedAt,
		UpdatedAt:         n.UpdatedAt,
	}
}

func convertPRs(nodes []gqlPR, viewer string) []PullRequest {
	prs := make([]PullRequest, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if isBot(n.Author.Type, n.Author.Login) {
			continue
		}
		prs = append(prs, convertCommonPR(n, viewer))
	}
	return prs
}

func isBot(typeName, login string) bool {
	return typeName == "Bot" || strings.Contains(login, "[bot]")
}

// parseGitHubResourceURL extracts owner/repo and number from a GitHub URL
// where parts[2] must equal segment (e.g. "pull" or "issues").
func parseGitHubResourceURL(rawURL, segment string) (repo string, number int) {
	if !strings.HasPrefix(rawURL, "https://github.com/") {
		return "", 0
	}
	parts := strings.Split(strings.TrimPrefix(rawURL, "https://github.com/"), "/")
	if len(parts) < 4 || parts[2] != segment {
		return "", 0
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n == 0 {
		return "", 0
	}
	return parts[0] + "/" + parts[1], n
}

// ParsePRURL extracts owner/repo and PR number from a GitHub pull request URL.
// Returns ("", 0) if the URL doesn't match.
func ParsePRURL(rawURL string) (repo string, number int) {
	return parseGitHubResourceURL(rawURL, "pull")
}

// ParseIssueURL extracts owner/repo and issue number from a GitHub issue URL.
// Returns ("", 0) if the URL doesn't match.
func ParseIssueURL(rawURL string) (repo string, number int) {
	return parseGitHubResourceURL(rawURL, "issues")
}

// ViewerLogin returns the authenticated GitHub user's login.
func ViewerLogin() string {
	return viewerLogin(defaultExecer)
}
