package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrBudgetExhausted signals the GraphQL points budget is too low to spend on
// an optional poll this cycle. Callers treat it like a transient error — skip
// the work, back off, retry next cycle — rather than firing a request that
// would only get a hard rate-limit rejection. IsTransientError reports true.
//
// GraphQL and REST are separate 5k/hr buckets; when GraphQL is exhausted the
// REST bucket is usually idle, so the per-PR monitor fetch falls back to REST
// (fetchPRForMonitorViaREST) for the conflict/CI/state signals instead of
// stalling. Review-thread data is GraphQL-only, so the REST path leaves those
// fields zero and callers must not drive thread-dependent actions (auto-merge,
// comments fixes) from a REST-sourced PR.
var ErrBudgetExhausted = errors.New("github graphql budget low: skipping optional poll")

// restPR is the subset of the GitHub REST pull-request payload the monitor
// needs for conflict/CI/state detection.
type restPR struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"` // open | closed
	Draft          bool   `json:"draft"`
	MergeableState string `json:"mergeable_state"` // clean|dirty|blocked|unstable|behind|unknown
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type restCheckRuns struct {
	CheckRuns []struct {
		Status     string `json:"status"`     // queued | in_progress | completed
		Conclusion string `json:"conclusion"` // success | failure | ...
	} `json:"check_runs"`
}

// fetchPRForMonitorViaREST fetches a PR's conflict/CI/state signals over the
// REST API (a separate rate-limit bucket from GraphQL). Review-decision and
// review-thread fields stay zero — REST does not expose thread resolution — so
// the caller must only act on conflict/ci_failure/closed signals from the
// result, never auto-merge or comments.
func fetchPRForMonitorViaREST(e execer, repo string, number int) (PullRequest, bool, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || number <= 0 {
		return PullRequest{}, false, fmt.Errorf("invalid repo or PR: %s#%d", repo, number)
	}

	resp, err := runGHAPIWith(e, "30s", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, name, number))
	if err != nil {
		return PullRequest{}, false, fmt.Errorf("gh api rest pr %s#%d: %s: %w", repo, number, sanitizeGHOutput(resp.body), err)
	}
	var pr restPR
	if err := json.Unmarshal(resp.body, &pr); err != nil {
		return PullRequest{}, false, fmt.Errorf("parse rest pr %s#%d: %w", repo, number, err)
	}
	if !strings.EqualFold(pr.State, "open") {
		return PullRequest{}, false, nil
	}

	ci, pending := fetchCIStatusViaREST(e, owner, name, pr.Head.SHA)

	out := PullRequest{
		Number:           pr.Number,
		Title:            pr.Title,
		URL:              pr.HTMLURL,
		Repository:       repo,
		RepoName:         name,
		Author:           pr.User.Login,
		IsDraft:          pr.Draft,
		HeadRefName:      pr.Head.Ref,
		HeadSHA:          pr.Head.SHA,
		Mergeable:        restMergeable(pr.MergeableState),
		CIStatus:         ci,
		HasPendingChecks: pending,
		CreatedAt:        pr.CreatedAt,
		UpdatedAt:        pr.UpdatedAt,
	}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	return out, true, nil
}

// restMergeable maps the REST mergeable_state to the monitor's Mergeable enum.
// Only "dirty" is a true merge conflict; "unknown"/"" means GitHub has not
// computed it yet (treat as UNKNOWN so we don't act); everything else
// (clean/blocked/unstable/behind) is not a conflict.
func restMergeable(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "dirty":
		return "CONFLICTING"
	case "", "unknown":
		return "UNKNOWN"
	default:
		return "MERGEABLE"
	}
}

// fetchCIStatusViaREST aggregates check-runs for a commit into the monitor's
// CIStatus semantics. Conservative: only a clear failing conclusion yields
// FAILURE; any incomplete run yields PENDING; all-complete-success yields
// SUCCESS; no checks yields "" (treated as not-failing upstream).
func fetchCIStatusViaREST(e execer, owner, name, sha string) (status string, pending bool) {
	if sha == "" {
		return "", false
	}
	resp, err := runGHAPIWith(e, "30s", fmt.Sprintf("repos/%s/%s/commits/%s/check-runs", owner, name, sha))
	if err != nil {
		return "", false
	}
	var runs restCheckRuns
	if jErr := json.Unmarshal(resp.body, &runs); jErr != nil || len(runs.CheckRuns) == 0 {
		return "", false
	}
	anyPending := false
	for _, c := range runs.CheckRuns {
		switch strings.ToLower(c.Conclusion) {
		case "failure", "timed_out", "cancelled", "action_required", "startup_failure":
			return "FAILURE", false
		}
		if !strings.EqualFold(c.Status, "completed") {
			anyPending = true
		}
	}
	if anyPending {
		return "PENDING", true
	}
	return "SUCCESS", false
}
