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
	return ghGate.execute(func() ([]byte, error) {
		cmd := exec.Command("gh", args...)
		return cmd.CombinedOutput()
	})
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

// sanitizeGHOutput trims the `gh` CLI's combined output for use in error
// messages. When GitHub returns a 5xx with an HTML error page (e.g. the
// "Unicorn!" 504 page), gh prints the entire HTML body followed by a
// "gh: HTTP <code>" status line. Returning that wall of HTML to the UI
// is unreadable, so collapse it to just the status line.
func sanitizeGHOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if !strings.Contains(s, "<!DOCTYPE html") && !strings.Contains(s, "<html") {
		return s
	}
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '\n' {
			continue
		}
		if line := strings.TrimSpace(s[i+1:]); strings.HasPrefix(line, "gh:") {
			return line
		}
	}
	return "GitHub returned an HTML error page"
}

const prQuery = `query($q: String!) {
  viewer { login }
  search(query: $q, type: ISSUE, first: 100) {
    pageInfo {
      hasNextPage
      endCursor
    }
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
              oid
              statusCheckRollup {
                state
                contexts(first: 50) {
                  nodes {
                    __typename
                    ... on CheckRun { name status conclusion }
                    ... on StatusContext { name: context state }
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

// gqlCheckContext captures both `CheckRun` and `StatusContext` shapes from
// the StatusCheckRollup contexts edge. The GraphQL query aliases
// `StatusContext.context` → `name` and only `CheckRun` populates
// status/conclusion, so callers must dispatch on Typename.
type gqlCheckContext struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // CheckRun only: QUEUED|IN_PROGRESS|COMPLETED|...
	Conclusion string `json:"conclusion"` // CheckRun only: SUCCESS|FAILURE|NEUTRAL|...
	State      string `json:"state"`      // StatusContext only: PENDING|SUCCESS|FAILURE|ERROR|EXPECTED
}

type gqlStatusCheckRollup struct {
	State    string `json:"state"`
	Contexts struct {
		Nodes []gqlCheckContext `json:"nodes"`
	} `json:"contexts"`
}

type gqlResponse struct {
	Data struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Search struct {
			Nodes    []gqlPR `json:"nodes"`
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
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
				OID               string                `json:"oid"`
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
	resp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+prQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(resp.body), err)
	}

	var gqlResp gqlResponse
	if err := json.Unmarshal(resp.body, &gqlResp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}

	return convertPRs(gqlResp.Data.Search.Nodes, gqlResp.Data.Viewer.Login), nil
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
	var headSHA string
	if len(n.Commits.Nodes) > 0 {
		headSHA = n.Commits.Nodes[0].Commit.OID
		if rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup; rollup != nil {
			// Recompute rollup from contexts so non-gating reporters
			// (codecov, sonarcloud) don't masquerade as CI failures and
			// drive pr-fix loops. Fall back to the GitHub rollup state
			// when no contexts came back (zero-checks PR or older gh).
			filtered, filteredPending := rollupFromContexts(rollup.Contexts.Nodes)
			if filtered != "" {
				ciStatus = filtered
				hasPendingChecks = filteredPending
			} else {
				ciStatus = rollup.State
				for _, ctx := range rollup.Contexts.Nodes {
					if ctx.Status != "" && ctx.Status != "COMPLETED" {
						hasPendingChecks = true
						break
					}
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
	var copilotReviewed bool
	for _, r := range n.LatestReviews.Nodes {
		if viewer != "" && strings.EqualFold(r.Author.Login, viewer) && r.State == "APPROVED" {
			viewerApproved = true
		}
		if IsCopilotReviewer(r.Author.Login) {
			copilotReviewed = true
		}
	}

	return PullRequest{
		Number:            n.Number,
		Title:             n.Title,
		URL:               n.URL,
		HeadRefName:       n.HeadRefName,
		HeadSHA:           headSHA,
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
		CopilotReviewed:   copilotReviewed,
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

// IsCopilotReviewer reports whether a review-author login belongs to GitHub
// Copilot's automated code reviewer. Copilot surfaces under a few first-party
// logins (Copilot, copilot-pull-request-reviewer[bot], github-copilot[bot]);
// match those exactly — not a substring or prefix — so a human or third-party
// login containing or starting with the word can't satisfy the merge gate.
func IsCopilotReviewer(login string) bool {
	switch strings.ToLower(login) {
	case "copilot",
		"copilot[bot]",
		"copilot-pull-request-reviewer",
		"copilot-pull-request-reviewer[bot]",
		"github-copilot[bot]":
		return true
	default:
		return false
	}
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

// IsTransientError reports whether err is a transient GitHub API failure
// (HTTP 5xx or network timeout) that is expected to resolve on its own.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// HTTP 5xx: sanitized gh output produces "gh: http 5xx"
	if strings.Contains(msg, "http 5") {
		return true
	}
	return strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "context deadline exceeded")
}
