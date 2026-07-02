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
        reviewThreads(first: 100) {
          nodes {
            id
            isResolved
            comments(last: 1) { nodes { author { login } } }
          }
        }
        latestReviews(first: 10) {
          nodes { state author { login } }
        }
      }
    }
  }
}`

const prForMonitorQuery = `query($owner: String!, $name: String!, $number: Int!) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      number
      title
      url
      state
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
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          comments(last: 1) { nodes { author { login } } }
        }
      }
      latestReviews(first: 10) {
        nodes { state author { login } }
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

type gqlPRForMonitorResponse struct {
	Data struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Repository struct {
			PullRequest *gqlPR `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// FetchReviews returns open PRs created by the user and review requests, excluding bots.
func FetchReviews() (ReviewSummary, error) {
	return fetchReviewsWith(defaultExecer)
}

// FetchPRForMonitor fetches one PR with the same signals used by FetchReviews,
// but without a GitHub search query. The bool reports whether the PR is still
// open; closed/merged PRs are left to FetchPRState-based reconciliation.
func FetchPRForMonitor(repo string, number int) (PullRequest, bool, error) {
	return fetchPRForMonitorWith(defaultExecer, repo, number)
}

func fetchPRForMonitorWith(e execer, repo string, number int) (PullRequest, bool, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || number <= 0 {
		return PullRequest{}, false, fmt.Errorf("invalid repo or PR: %s#%d", repo, number)
	}

	// When the GraphQL budget is low, fetch the conflict/CI/state signals over
	// REST (a separate, usually-idle bucket) instead of spending scarce GraphQL
	// points or hard-failing. The REST result omits thread/review-decision data,
	// so callers must only act on its conflict/ci_failure/closed signals. Gated
	// on runtimeCacheEnabled so unit tests (fake execer) keep the GraphQL path.
	if runtimeCacheEnabled(e) && ghGate.shouldSkipOptional("graphql", priorityMergePath) {
		return fetchPRForMonitorViaREST(e, repo, number)
	}

	resp, err := runGHAPIWith(e, "", "graphql",
		"-f", "query="+prForMonitorQuery,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-F", "number="+strconv.Itoa(number))
	if err != nil {
		return PullRequest{}, false, fmt.Errorf("gh api graphql pr %s#%d: %s: %w", repo, number, sanitizeGHOutput(resp.body), err)
	}

	var gqlResp gqlPRForMonitorResponse
	if err := json.Unmarshal(resp.body, &gqlResp); err != nil {
		return PullRequest{}, false, fmt.Errorf("parse graphql pr response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return PullRequest{}, false, fmt.Errorf("graphql pr %s#%d: %s", repo, number, gqlResp.Errors[0].Message)
	}
	if gqlResp.Data.Repository.PullRequest == nil {
		return PullRequest{}, false, nil
	}
	pr := gqlResp.Data.Repository.PullRequest
	if pr.State != "OPEN" {
		return PullRequest{}, false, nil
	}
	return convertCommonPR(pr, gqlResp.Data.Viewer.Login), true, nil
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
		// Pace the search legs by the live GraphQL budget: when it is low, serve
		// a stale summary if we have one, otherwise back off (ErrBudgetExhausted
		// is transient) instead of firing three searches that would only be
		// rejected and burn the last of the shared budget.
		if ghGate.shouldSkipOptional("graphql", priorityDiscovery) {
			if stale, ok := reviewSummaryCache.GetStale(cacheKey); ok {
				return stale, nil
			}
			return ReviewSummary{}, ErrBudgetExhausted
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
		reviewSummaryCache.Set(cacheKey, summary, 2*time.Minute)
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

// MyReviewState summarises the authenticated user's own reviews on a PR.
type MyReviewState struct {
	Pending     bool   // an unsubmitted draft review exists
	Submitted   bool   // a submitted (non-draft) review exists
	Approved    bool   // the latest submitted review is an approval
	ReviewedSHA string // commit_id of the latest submitted review ("" if none)
}

// FetchMyReviewState reports the authenticated user's review state on a PR.
// It reads the per-PR reviews REST endpoint, which — unlike the
// reviewed-by:@me search leg (filtered to approvals) — exposes COMMENTED and
// CHANGES_REQUESTED reviews and each review's commit_id, the signals the
// PR-review lifecycle needs.
func FetchMyReviewState(repo string, number int) (MyReviewState, error) {
	return fetchMyReviewStateWith(defaultExecer, repo, number)
}

func fetchMyReviewStateWith(e execer, repo string, number int) (MyReviewState, error) {
	key := prCacheKey(repo, number)
	if runtimeCacheEnabled(e) {
		if cached, ok := myReviewStateCache.Get(key); ok {
			return cached, nil
		}
	}

	resp, err := runGHAPIWith(e, "30s", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number))
	if err != nil {
		if runtimeCacheEnabled(e) {
			if stale, ok := myReviewStateCache.GetStale(key); ok {
				return stale, nil
			}
		}
		return MyReviewState{}, fmt.Errorf("fetch reviews for %s#%d: %s: %w", repo, number, strings.TrimSpace(string(resp.body)), err)
	}

	var reviews []struct {
		State       string `json:"state"`
		CommitID    string `json:"commit_id"`
		SubmittedAt string `json:"submitted_at"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(resp.body, &reviews); err != nil {
		return MyReviewState{}, fmt.Errorf("parse reviews: %w", err)
	}

	me := viewerLogin(e)
	if me == "" {
		// Without the viewer's login we cannot attribute submitted reviews, and
		// guessing would misclassify the phase. Surface a (transient) error so
		// the caller leaves the task's phase untouched this cycle rather than
		// flapping it to "manual".
		return MyReviewState{}, fmt.Errorf("resolve viewer login for %s#%d", repo, number)
	}

	var st MyReviewState
	var latestReview, latestVerdict string // submitted_at watermarks
	for i := range reviews {
		r := reviews[i]
		// PENDING (draft) reviews are visible only to their author, so any we
		// can see are ours.
		if r.State == "PENDING" {
			st.Pending = true
			continue
		}
		// Submitted reviews include every reviewer's — keep only the viewer's.
		if r.User.Login != me {
			continue
		}
		st.Submitted = true
		// Track the commit of the most recent review of any kind, for
		// push-past-reviewed-commit detection. ISO-8601 sorts lexically.
		if r.SubmittedAt >= latestReview {
			latestReview = r.SubmittedAt
			st.ReviewedSHA = r.CommitID
		}
		// Only APPROVED/CHANGES_REQUESTED/DISMISSED carry a standing verdict; a
		// COMMENTED review left after an approval does not revoke it.
		if r.State == "APPROVED" || r.State == "CHANGES_REQUESTED" || r.State == "DISMISSED" {
			if r.SubmittedAt >= latestVerdict {
				latestVerdict = r.SubmittedAt
				st.Approved = r.State == "APPROVED"
			}
		}
	}

	if runtimeCacheEnabled(e) {
		myReviewStateCache.Set(key, st, 30*time.Second)
	}
	return st, nil
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
