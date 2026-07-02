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
        baseRefName
        isDraft
        mergeable
        createdAt
        updatedAt
        reviewDecision
        autoMergeRequest { enabledAt }
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

// monitorPRFields is the field selection shared by the single-PR and batched
// monitor queries, kept in one place so the two paths can never drift apart.
const monitorPRFields = `
      number
      title
      url
      state
      headRefName
      baseRefName
      isDraft
      mergeable
      createdAt
      updatedAt
      reviewDecision
      autoMergeRequest { enabledAt }
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
      }`

const prForMonitorQuery = `query($owner: String!, $name: String!, $number: Int!) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {` + monitorPRFields + `
    }
  }
}`

// maxBatchPRsPerQuery caps how many PRs are aliased into a single batched
// monitor query, staying under GitHub's per-request node/complexity limits.
const maxBatchPRsPerQuery = 20

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

// gqlBatchPRResponse decodes a batched monitor query. Each requested PR gets
// its own top-level aliased field (repo0, repo1, ...), so Data is decoded as
// a raw map instead of a fixed struct.
type gqlBatchPRResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Path    []any  `json:"path"`
	} `json:"errors"`
}

// aliasErrors buckets the response's errors by the top-level aliased field
// (e.g. "repo3") their path points at. Errors without a path, whose path
// doesn't start with a string, or whose path points at a field other than a
// "repoN" ref alias (e.g. the shared "viewer" field) are global: they apply
// to every ref since GitHub returned no usable per-ref distinction, or the
// error is scoped to a field every ref's result depends on.
func (r *gqlBatchPRResponse) aliasErrors() (perAlias map[string]string, global string) {
	perAlias = make(map[string]string)
	for _, ge := range r.Errors {
		if len(ge.Path) == 0 {
			global = ge.Message
			continue
		}
		alias, ok := ge.Path[0].(string)
		if !ok || !isRefAlias(alias) {
			global = ge.Message
			continue
		}
		if _, exists := perAlias[alias]; !exists {
			perAlias[alias] = ge.Message
		}
	}
	return perAlias, global
}

// isRefAlias reports whether alias has the "repoN" shape used for per-ref
// aliases in the batched monitor query (as opposed to shared top-level
// fields like "viewer").
func isRefAlias(alias string) bool {
	rest, ok := strings.CutPrefix(alias, "repo")
	if !ok || rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (r *gqlBatchPRResponse) viewerLogin() string {
	raw, ok := r.Data["viewer"]
	if !ok {
		return ""
	}
	var v struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v.Login
}

type gqlBatchRepoNode struct {
	PullRequest *gqlPR `json:"pullRequest"`
}

// PRRef identifies a single PR to fetch in a batched monitor request.
type PRRef struct {
	Repo   string
	Number int
}

// MonitorPRResult is one PR's outcome from a batched monitor fetch. Open
// mirrors the bool FetchPRForMonitor returns: false for closed/merged PRs
// (left to FetchPRState-based reconciliation) as well as fetch errors.
type MonitorPRResult struct {
	Repo   string
	Number int
	PR     PullRequest
	Open   bool
	Err    error
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

// FetchPRsForMonitor fetches multiple PRs' monitor signals, aliasing them
// into as few GraphQL requests as possible (chunked to stay under GitHub's
// node/complexity limits) instead of issuing one query per PR. Results are
// returned in the same order as refs.
func FetchPRsForMonitor(refs []PRRef) []MonitorPRResult {
	return fetchPRsForMonitorWith(defaultExecer, refs)
}

func fetchPRsForMonitorWith(e execer, refs []PRRef) []MonitorPRResult {
	if len(refs) == 0 {
		return nil
	}

	// When the GraphQL budget is low, keep the existing single-PR REST
	// fallback per ref instead of batching — same behavior/data tradeoffs as
	// fetchPRForMonitorWith. Gated on runtimeCacheEnabled so unit tests (fake
	// execer) keep the GraphQL path.
	if runtimeCacheEnabled(e) && ghGate.shouldSkipOptional("graphql", priorityMergePath) {
		results := make([]MonitorPRResult, len(refs))
		for i, ref := range refs {
			pr, open, err := fetchPRForMonitorViaREST(e, ref.Repo, ref.Number)
			results[i] = MonitorPRResult{Repo: ref.Repo, Number: ref.Number, PR: pr, Open: open, Err: err}
		}
		return results
	}

	results := make([]MonitorPRResult, 0, len(refs))
	cacheEnabled := runtimeCacheEnabled(e)
	for start := 0; start < len(refs); start += maxBatchPRsPerQuery {
		end := min(start+maxBatchPRsPerQuery, len(refs))
		// Recheck the gate before every chunk: an earlier chunk in this same
		// loop can observe rate-limit headers (via runGHAPIWith) and trip the
		// gate into a low-budget state, in which case the remaining refs
		// should fall back to REST instead of continuing to spend GraphQL
		// budget or hitting rate-limit errors.
		if cacheEnabled && ghGate.shouldSkipOptional("graphql", priorityMergePath) {
			for _, ref := range refs[start:] {
				pr, open, err := fetchPRForMonitorViaREST(e, ref.Repo, ref.Number)
				results = append(results, MonitorPRResult{Repo: ref.Repo, Number: ref.Number, PR: pr, Open: open, Err: err})
			}
			return results
		}
		results = append(results, fetchPRBatchWith(e, refs[start:end])...)
	}
	return results
}

// fetchPRBatchWith fetches one chunk of refs (already capped to
// maxBatchPRsPerQuery) in a single GraphQL request, aliasing
// repository(owner,name){ pullRequest(number) } per ref.
func fetchPRBatchWith(e execer, refs []PRRef) []MonitorPRResult {
	type validRef struct {
		idx    int // index into refs/results
		owner  string
		name   string
		number int
	}
	results := make([]MonitorPRResult, len(refs))
	valid := make([]validRef, 0, len(refs))
	for i, ref := range refs {
		results[i] = MonitorPRResult{Repo: ref.Repo, Number: ref.Number}
		owner, name, ok := strings.Cut(ref.Repo, "/")
		if !ok || owner == "" || name == "" || ref.Number <= 0 {
			results[i].Err = fmt.Errorf("invalid repo or PR: %s#%d", ref.Repo, ref.Number)
			continue
		}
		valid = append(valid, validRef{idx: i, owner: owner, name: name, number: ref.Number})
	}
	if len(valid) == 0 {
		return results
	}

	var query strings.Builder
	query.WriteString("query(")
	for j := range valid {
		if j > 0 {
			query.WriteString(", ")
		}
		fmt.Fprintf(&query, "$owner%d: String!, $name%d: String!, $number%d: Int!", j, j, j)
	}
	query.WriteString(") {\n  viewer { login }\n")
	for j := range valid {
		fmt.Fprintf(&query, "  repo%d: repository(owner: $owner%d, name: $name%d) {\n    pullRequest(number: $number%d) {%s\n    }\n  }\n", j, j, j, j, monitorPRFields)
	}
	query.WriteString("}")

	args := make([]string, 0, 4+len(valid)*6)
	args = append(args, "graphql", "-f", "query="+query.String())
	for j, v := range valid {
		args = append(args,
			"-f", fmt.Sprintf("owner%d=%s", j, v.owner),
			"-f", fmt.Sprintf("name%d=%s", j, v.name),
			"-F", fmt.Sprintf("number%d=%d", j, v.number),
		)
	}

	resp, err := runGHAPIWith(e, "", args...)
	if err != nil {
		batchErr := fmt.Errorf("gh api graphql pr batch: %s: %w", sanitizeGHOutput(resp.body), err)
		for _, v := range valid {
			results[v.idx].Err = batchErr
		}
		return results
	}

	var gqlResp gqlBatchPRResponse
	if err := json.Unmarshal(resp.body, &gqlResp); err != nil {
		parseErr := fmt.Errorf("parse graphql pr batch response: %w", err)
		for _, v := range valid {
			results[v.idx].Err = parseErr
		}
		return results
	}
	// GitHub GraphQL can return partial data: an error scoped to one alias
	// (e.g. a deleted/inaccessible repo or PR) alongside valid data for every
	// other alias in the batch. Only fail the aliases named in a per-alias
	// error path; a query-level error with no path (global) still fails the
	// whole batch, since there's no per-alias data to salvage.
	perAliasErr, globalErr := gqlResp.aliasErrors()
	if globalErr != "" {
		gqlErr := fmt.Errorf("graphql pr batch: %s", globalErr)
		for _, v := range valid {
			results[v.idx].Err = gqlErr
		}
		return results
	}

	viewer := gqlResp.viewerLogin()
	for j, v := range valid {
		alias := fmt.Sprintf("repo%d", j)
		if msg, failed := perAliasErr[alias]; failed {
			results[v.idx].Err = fmt.Errorf("graphql pr batch %s: %s", alias, msg)
			continue
		}
		raw, ok := gqlResp.Data[alias]
		if !ok {
			results[v.idx].Err = fmt.Errorf("graphql pr batch: missing %s in response", alias)
			continue
		}
		var node gqlBatchRepoNode
		if err := json.Unmarshal(raw, &node); err != nil {
			results[v.idx].Err = fmt.Errorf("parse graphql pr batch %s: %w", alias, err)
			continue
		}
		if node.PullRequest == nil || node.PullRequest.State != "OPEN" {
			continue
		}
		results[v.idx].PR = convertCommonPR(node.PullRequest, viewer)
		results[v.idx].Open = true
	}
	return results
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
