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

	return convertPRs(resp.Data.Search.Nodes), nil
}

func convertPRs(nodes []gqlPR) []PullRequest {
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

		prs = append(prs, PullRequest{
			Number:          n.Number,
			Title:           n.Title,
			URL:             n.URL,
			HeadRefName:     n.HeadRefName,
			Repository:      n.Repository.NameWithOwner,
			RepoName:        n.Repository.Name,
			Author:          n.Author.Login,
			IsDraft:         n.IsDraft,
			Mergeable:       n.Mergeable,
			Labels:          labels,
			CIStatus:        ciStatus,
			ReviewDecision:  n.ReviewDecision,
			UnresolvedCount: unresolved,
			CreatedAt:       n.CreatedAt,
			UpdatedAt:       n.UpdatedAt,
		})
	}
	return prs
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

// PRState holds the current state of a specific PR.
type PRState struct {
	State    string `json:"state"`    // OPEN, CLOSED, MERGED
	MergedAt string `json:"mergedAt"` // non-empty if merged
}

// FetchPRState fetches the current state of a specific PR by repo and number.
func FetchPRState(repo string, number int) (PRState, error) {
	return fetchPRStateWith(defaultExecer, repo, number)
}

func fetchPRStateWith(e execer, repo string, number int) (PRState, error) {
	out, err := e.run("pr", "view", fmt.Sprintf("%d", number),
		"--repo", repo, "--json", "state,mergedAt")
	if err != nil {
		return PRState{}, fmt.Errorf("gh pr view %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	var s PRState
	if err := json.Unmarshal(out, &s); err != nil {
		return PRState{}, fmt.Errorf("parse pr state: %w", err)
	}
	return s, nil
}
