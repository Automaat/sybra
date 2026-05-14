package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// reviewSummaryQuery fetches one PR search leg. FetchReviews runs it once per
// query instead of fanning out multiple search legs in one GraphQL request; the
// combined form times out at GitHub's edge for accounts with many open review
// requests.
const reviewSummaryQuery = `query($q: String!) {
  viewer { login }
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
        labels(first: 5) { nodes { name } }
        commits(last: 1) {
          nodes {
            commit {
              oid
              statusCheckRollup {
                state
                contexts(first: 20) {
                  nodes {
                    __typename
                    ... on CheckRun {
                      name
                      status
                      conclusion
                    }
                    ... on StatusContext {
                      name: context
                      state
                    }
                  }
                }
              }
            }
          }
        }
        reviewThreads(first: 20) {
          nodes { isResolved }
        }
        latestReviews(first: 10) {
          nodes { state author { login } }
        }
      }
    }
  }
}`

type gqlReviewSummaryResponse struct {
	Data struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Search struct {
			Nodes []gqlPR `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// FetchReviews returns open PRs created by the user and review requests, excluding bots.
func FetchReviews() (ReviewSummary, error) {
	return fetchReviewsWith(defaultExecer)
}

func fetchReviewsWith(e execer) (ReviewSummary, error) {
	const (
		createdQuery   = "is:pr is:open author:@me"
		requestedQuery = "is:pr is:open review-requested:@me"
		reviewedQuery  = "is:pr is:open reviewed-by:@me"
	)
	cacheKey := createdQuery + "||" + requestedQuery + "||" + reviewedQuery
	if runtimeCacheEnabled(e) {
		if cached, ok := reviewSummaryCache.Get(cacheKey); ok {
			return cached, nil
		}
		if ghGate.shouldSkipOptional("graphql") {
			if stale, ok := reviewSummaryCache.GetStale(cacheKey); ok {
				return stale, nil
			}
		}
	}

	created, err := fetchReviewSearchWith(e, createdQuery)
	if err != nil {
		return staleReviewSummaryOrError(e, cacheKey, "created", err)
	}
	requested, err := fetchReviewSearchWith(e, requestedQuery)
	if err != nil {
		return staleReviewSummaryOrError(e, cacheKey, "requested", err)
	}
	reviewed, err := fetchReviewSearchWith(e, reviewedQuery)
	if err != nil {
		return staleReviewSummaryOrError(e, cacheKey, "reviewed", err)
	}

	summary := ReviewSummary{
		CreatedByMe:     created,
		ReviewRequested: requested,
		ReviewedByMe:    approvedOnly(reviewed),
	}
	if runtimeCacheEnabled(e) {
		reviewSummaryCache.Set(cacheKey, summary, 20*time.Second)
	}

	return summary, nil
}

func staleReviewSummaryOrError(e execer, cacheKey, leg string, err error) (ReviewSummary, error) {
	if runtimeCacheEnabled(e) {
		if stale, ok := reviewSummaryCache.GetStale(cacheKey); ok {
			return stale, nil
		}
	}
	return ReviewSummary{}, fmt.Errorf("fetch %s reviews: %w", leg, err)
}

func fetchReviewSearchWith(e execer, query string) ([]PullRequest, error) {
	resp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+reviewSummaryQuery,
		"-f", "q="+query)
	if err != nil {
		return nil, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(resp.body), err)
	}

	var gqlResp gqlReviewSummaryResponse
	if err := json.Unmarshal(resp.body, &gqlResp); err != nil {
		return nil, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}

	return convertPRs(gqlResp.Data.Search.Nodes, gqlResp.Data.Viewer.Login), nil
}

func approvedOnly(prs []PullRequest) []PullRequest {
	out := prs[:0]
	for i := range prs {
		if prs[i].ViewerHasApproved {
			out = append(out, prs[i])
		}
	}
	return out
}

// HasPendingReview checks if the authenticated user has a pending (draft) review on a PR.
// Pending reviews are only visible to their author via the REST API.
func HasPendingReview(repo string, number int) (bool, error) {
	return hasPendingReviewWith(defaultExecer, repo, number)
}

func hasPendingReviewWith(e execer, repo string, number int) (bool, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := pendingReviewCache.Get(key); ok {
			return cached, nil
		}
	}

	resp, err := runGHAPIWith(e, "30s", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number))
	if err != nil {
		if runtimeCacheEnabled(e) {
			if stale, ok := pendingReviewCache.GetStale(key); ok {
				return stale, nil
			}
		}
		return false, fmt.Errorf("fetch reviews for %s#%d: %s: %w", repo, number, strings.TrimSpace(string(resp.body)), err)
	}
	var reviews []struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(resp.body, &reviews); err != nil {
		return false, fmt.Errorf("parse reviews: %w", err)
	}
	for i := range reviews {
		if reviews[i].State == "PENDING" {
			if runtimeCacheEnabled(e) {
				pendingReviewCache.Set(key, true, 30*time.Second)
			}
			return true, nil
		}
	}
	if runtimeCacheEnabled(e) {
		pendingReviewCache.Set(key, false, 30*time.Second)
	}
	return false, nil
}

// ApprovePR approves a pull request.
func ApprovePR(repo string, number int) error {
	return approvePRWith(defaultExecer, repo, number)
}

func approvePRWith(e execer, repo string, number int) error {
	out, err := e.run("pr", "review", "--approve",
		strconv.Itoa(number), "-R", repo)
	if err != nil {
		return fmt.Errorf("gh pr review --approve %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	if runtimeCacheEnabled(e) {
		invalidatePRCaches(repo, number)
	}
	return nil
}
