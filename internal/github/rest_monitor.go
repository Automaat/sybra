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
	MergedAt       string `json:"merged_at"`
	Draft          bool   `json:"draft"`
	MergeableState string `json:"mergeable_state"` // clean|dirty|blocked|unstable|behind|unknown
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// AutoMerge is non-nil when GitHub's native auto-merge is armed.
	AutoMerge *struct {
		EnabledBy struct {
			Login string `json:"login"`
		} `json:"enabled_by"`
	} `json:"auto_merge"`
}

type restCheckRuns struct {
	CheckRuns []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`     // queued | in_progress | completed
		Conclusion string `json:"conclusion"` // success | failure | ...
	} `json:"check_runs"`
}

type restCommitStatuses struct {
	Statuses []struct {
		Context string `json:"context"`
		State   string `json:"state"` // error | failure | pending | success
	} `json:"statuses"`
}

// fetchPRForMonitorViaREST fetches a PR's conflict/CI/state signals over the
// REST API (a separate rate-limit bucket from GraphQL). Review-decision and
// review-thread fields stay zero — REST does not expose thread resolution — so
// the caller must only act on conflict/ci_failure/closed signals from the
// result, never auto-merge or comments.
//
// REST-sourced PRs also carry zero Copilot-review and unresolved-thread
// fields (CopilotReviewed, UnresolvedCount, ActionableCount), so a caller must
// never use one to decide whether to ARM native auto-merge — only to observe
// already-known state (e.g. AutoMergeEnabled already armed, or a terminal
// MERGED/CLOSED transition).
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
		BaseRefName:      pr.Base.Ref,
		Mergeable:        restMergeable(pr.MergeableState),
		CIStatus:         ci,
		HasPendingChecks: pending,
		AutoMergeEnabled: pr.AutoMerge != nil,
		CreatedAt:        pr.CreatedAt,
		UpdatedAt:        pr.UpdatedAt,
	}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	return out, true, nil
}

// FetchPRStateViaREST fetches a PR's terminal/open state over GitHub's REST
// API. It is used when the GraphQL budget is exhausted, so closed-task
// reconciliation does not immediately re-enter the GraphQL-backed `gh pr view`
// path.
func FetchPRStateViaREST(repo string, number int) (PRState, error) {
	return fetchPRStateViaREST(defaultExecer, repo, number)
}

func fetchPRStateViaREST(e execer, repo string, number int) (PRState, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || number <= 0 {
		return PRState{}, fmt.Errorf("invalid repo or PR: %s#%d", repo, number)
	}

	resp, err := runGHAPIWith(e, "30s", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, name, number))
	if err != nil {
		return PRState{}, fmt.Errorf("gh api rest pr state %s#%d: %s: %w", repo, number, sanitizeGHOutput(resp.body), err)
	}
	var pr restPR
	if err := json.Unmarshal(resp.body, &pr); err != nil {
		return PRState{}, fmt.Errorf("parse rest pr state %s#%d: %w", repo, number, err)
	}

	state := strings.ToUpper(strings.TrimSpace(pr.State))
	if state == "CLOSED" && strings.TrimSpace(pr.MergedAt) != "" {
		state = "MERGED"
	}
	return PRState{
		State:     state,
		MergedAt:  pr.MergedAt,
		Mergeable: restMergeable(pr.MergeableState),
	}, nil
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

// fetchCIStatusViaREST aggregates check-runs and legacy commit statuses for a
// commit into the monitor's CIStatus semantics. It converts REST payloads into
// the same context shape used by GraphQL so informational-check filtering and
// cancelled-check handling stay identical across both paths.
func fetchCIStatusViaREST(e execer, owner, name, sha string) (status string, pending bool) {
	if sha == "" {
		return "", false
	}
	contexts := make([]gqlCheckContext, 0)

	resp, err := runGHAPIWith(e, "30s", fmt.Sprintf("repos/%s/%s/commits/%s/check-runs", owner, name, sha))
	if err == nil {
		var runs restCheckRuns
		if jErr := json.Unmarshal(resp.body, &runs); jErr == nil {
			for _, c := range runs.CheckRuns {
				contexts = append(contexts, gqlCheckContext{
					Typename:   "CheckRun",
					Name:       c.Name,
					Status:     strings.ToUpper(c.Status),
					Conclusion: strings.ToUpper(c.Conclusion),
				})
			}
		}
	}

	resp, err = runGHAPIWith(e, "30s", fmt.Sprintf("repos/%s/%s/commits/%s/status", owner, name, sha))
	if err == nil {
		var statuses restCommitStatuses
		if jErr := json.Unmarshal(resp.body, &statuses); jErr == nil {
			for _, s := range statuses.Statuses {
				contexts = append(contexts, gqlCheckContext{
					Typename: "StatusContext",
					Name:     s.Context,
					State:    strings.ToUpper(s.State),
				})
			}
		}
	}

	return rollupFromContexts(contexts)
}
