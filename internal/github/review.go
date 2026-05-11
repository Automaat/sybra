package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// reviewSummaryQuery fans out two PR searches in one request.
//
// Caps were tightened after GitHub's GraphQL edge consistently 502'd this
// query for accounts with non-trivial PR sets. The original `first:100`
// search × deep `reviewThreads(first:100) + latestReviews(first:20) +
// contexts(first:50) + labels(first:10)` blew through ~36K complexity
// points before the doubling, sitting near GitHub's ~50K cutoff. The caps
// below preserve every consumer (UnresolvedCount, ViewerHasApproved,
// HasPendingChecks, Labels) but slash complexity ~85%.
const reviewSummaryQuery = `query($createdQ: String!, $requestedQ: String!) {
  viewer { login }
  created: search(query: $createdQ, type: ISSUE, first: 50) {
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
  requested: search(query: $requestedQ, type: ISSUE, first: 50) {
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
		Created struct {
			Nodes []gqlPR `json:"nodes"`
		} `json:"created"`
		Requested struct {
			Nodes []gqlPR `json:"nodes"`
		} `json:"requested"`
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
	)
	cacheKey := createdQuery + "||" + requestedQuery
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

	resp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+reviewSummaryQuery,
		"-f", "createdQ="+createdQuery,
		"-f", "requestedQ="+requestedQuery)
	if err != nil {
		if runtimeCacheEnabled(e) {
			if stale, ok := reviewSummaryCache.GetStale(cacheKey); ok {
				return stale, nil
			}
		}
		return ReviewSummary{}, fmt.Errorf("gh api graphql: %s: %w", sanitizeGHOutput(resp.body), err)
	}

	var gqlResp gqlReviewSummaryResponse
	if err := json.Unmarshal(resp.body, &gqlResp); err != nil {
		return ReviewSummary{}, fmt.Errorf("parse graphql response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return ReviewSummary{}, fmt.Errorf("graphql: %s", gqlResp.Errors[0].Message)
	}

	summary := ReviewSummary{
		CreatedByMe:     convertPRs(gqlResp.Data.Created.Nodes, gqlResp.Data.Viewer.Login),
		ReviewRequested: convertPRs(gqlResp.Data.Requested.Nodes, gqlResp.Data.Viewer.Login),
	}
	if runtimeCacheEnabled(e) {
		reviewSummaryCache.Set(cacheKey, summary, 20*time.Second)
	}

	return summary, nil
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
		fmt.Sprintf("%d", number), "-R", repo)
	if err != nil {
		return fmt.Errorf("gh pr review --approve %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	if runtimeCacheEnabled(e) {
		invalidatePRCaches(repo, number)
	}
	return nil
}
