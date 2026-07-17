package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// execer abstracts command execution for testing.
type execer interface {
	run(args ...string) ([]byte, error)
}

type ghExecer struct{}

func (ghExecer) run(args ...string) ([]byte, error) {
	return ghGate.execute(func() ([]byte, error) {
		// context.Background(): this is the plain, uncancellable fallback path
		// (see runE below) — callers that want cancellation use runCtx/ghRunCtx.
		cmd := exec.CommandContext(context.Background(), "gh", args...)
		if env := ghEnv(); env != nil {
			cmd.Env = env
		}
		return cmd.CombinedOutput()
	})
}

// ghRunCtx runs gh under a context so a stalled call is killed when the context
// expires — releasing the global request gate instead of holding it for the
// kernel TCP timeout. Used by latency-sensitive callers (the PR poll loop).
func ghRunCtx(ctx context.Context, args ...string) ([]byte, error) {
	return ghGate.execute(func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "gh", args...)
		if env := ghEnv(); env != nil {
			cmd.Env = env
		}
		return cmd.CombinedOutput()
	})
}

// runCtx lets ghExecer satisfy the optional ctxRunner interface, so callers
// that thread a context (e.g. the umbrella fetch from the poll loop) get a
// cancellable gh invocation instead of one bounded only by the kernel TCP
// timeout.
func (ghExecer) runCtx(ctx context.Context, args ...string) ([]byte, error) {
	return ghRunCtx(ctx, args...)
}

// ctxRunner is the optional context-aware extension of execer. The real
// ghExecer implements it; test fakes need not — runE falls back to run.
type ctxRunner interface {
	runCtx(ctx context.Context, args ...string) ([]byte, error)
}

// runE executes args on e, using the context-aware path when ctx is non-nil
// and e supports it; otherwise it falls back to the plain (uncancellable) run.
func runE(ctx context.Context, e execer, args ...string) ([]byte, error) {
	if ctx != nil {
		if cr, ok := e.(ctxRunner); ok {
			return cr.runCtx(ctx, args...)
		}
	}
	return e.run(args...)
}

var defaultExecer execer = ghExecer{}

var (
	viewerMu     sync.RWMutex
	cachedViewer string
	// viewerGen invalidates in-flight resolutions across an auth-mode switch.
	// The identity is mode-dependent, and resolving it is slow (a gh subprocess
	// or an HTTPS round-trip), so a resolution that started under the old mode
	// can finish after resetCachedViewer() and write a now-wrong login back
	// into an empty cache. That poisoned value looks like success, so nothing
	// downstream fails closed — see the write-back guard below.
	viewerGen uint64
)

// resetCachedViewer drops the memoized viewer login. Called when the auth mode
// changes, since the identity is resolved differently per mode.
func resetCachedViewer() {
	viewerMu.Lock()
	cachedViewer = ""
	viewerGen++
	viewerMu.Unlock()
}

func viewerLogin(ctx context.Context, e execer) string {
	login, err := viewerLoginE(ctx, e)
	if err != nil {
		return ""
	}
	return login
}

// viewerLoginE resolves the login this process acts as, returning the cause on
// failure so callers can report *why* attribution is impossible rather than a
// bare "unknown".
//
// Auth-mode-dependent by necessity: under GitHub App auth the identity is
// "<slug>[bot]" and must come from GET /app, because /user is a user-to-server
// endpoint that always 403s for installation tokens (the same trap #2032 hit in
// Authenticated() — see the comment there). Silently returning "" from a failed
// /user call is what froze review_phase and drove the 112-review loop in #2164.
func viewerLoginE(ctx context.Context, e execer) (string, error) {
	viewerMu.RLock()
	cached := cachedViewer
	gen := viewerGen
	viewerMu.RUnlock()
	if cached != "" {
		return cached, nil
	}

	var login string
	if src := currentAppSource(); src != nil {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		resolved, err := src.appLogin(reqCtx)
		if err != nil {
			return "", err
		}
		login = resolved
	} else {
		out, err := runE(ctx, e, "api", "user", "-q", ".login")
		if err != nil {
			return "", fmt.Errorf("gh api user: %w", err)
		}
		login = strings.TrimSpace(string(out))
		if login == "" {
			return "", fmt.Errorf("gh api user: empty login")
		}
	}

	viewerMu.Lock()
	defer viewerMu.Unlock()
	if viewerGen != gen {
		// The auth mode changed while we were resolving, so `login` describes
		// the old mode. Writing it back would defeat resetCachedViewer() and
		// pin a wrong-but-plausible identity forever.
		//
		// A non-empty cache here was necessarily written after the reset that
		// bumped the generation (the reset empties it), so it already reflects
		// the current mode and is safe to return. Otherwise fail: the caller
		// leaves state untouched and the next call resolves under the new mode.
		if cachedViewer != "" {
			return cachedViewer, nil
		}
		return "", fmt.Errorf("resolve viewer login: auth mode changed during resolution")
	}
	// Double-checked: another goroutine may have populated the cache
	// between RUnlock and Lock; keep whichever value is set.
	if cachedViewer == "" {
		cachedViewer = login
	}
	return cachedViewer, nil
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
          nodes {
            id
            isResolved
            comments(last: 1) { nodes { author { login } } }
          }
        }
        latestReviews(first: 20) {
          nodes { state author { login } }
        }
      }
    }
  }
}`

// gqlCheckContext captures both `CheckRun` and `StatusContext` shapes from
// the StatusCheckRollup contexts edge. The prQuery GraphQL fragment in this
// file aliases `StatusContext.context` → `name`, but `gh pr view --json
// statusCheckRollup` (used by FetchPRState) emits the raw field name
// `context` instead — both are captured so effectiveName() works for either
// source. Only `CheckRun` populates status/conclusion, so callers must
// dispatch on Typename.
type gqlCheckContext struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`    // StatusContext, non-GraphQL-aliased sources only
	Status     string `json:"status"`     // CheckRun only: QUEUED|IN_PROGRESS|COMPLETED|...
	Conclusion string `json:"conclusion"` // CheckRun only: SUCCESS|FAILURE|NEUTRAL|...
	State      string `json:"state"`      // StatusContext only: PENDING|SUCCESS|FAILURE|ERROR|EXPECTED
}

// effectiveName returns the check's display name regardless of which JSON
// shape populated it (GraphQL-aliased `name`, or raw `context`).
func (c gqlCheckContext) effectiveName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Context
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
	State          string `json:"state"`
	HeadRefName    string `json:"headRefName"`
	BaseRefName    string `json:"baseRefName"`
	IsDraft        bool   `json:"isDraft"`
	Mergeable      string `json:"mergeable"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	ReviewDecision string `json:"reviewDecision"`
	// AutoMergeRequest is non-nil when GitHub's native auto-merge is armed.
	AutoMergeRequest *struct {
		EnabledAt string `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	Author struct {
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
			ID         string `json:"id"`
			IsResolved bool   `json:"isResolved"`
			Comments   struct {
				Nodes []struct {
					Author struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"comments"`
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

	// The fix agent acts as the authenticated user (viewer). On own-PRs that
	// equals the PR author; fall back to it when the viewer lookup failed.
	agentLogin := viewer
	if agentLogin == "" {
		agentLogin = n.Author.Login
	}
	var unresolved, actionable int
	var sigTokens []string
	for i := range n.ReviewThreads.Nodes {
		th := &n.ReviewThreads.Nodes[i]
		if th.IsResolved {
			continue
		}
		unresolved++
		var lastAuthor string
		if len(th.Comments.Nodes) > 0 {
			lastAuthor = th.Comments.Nodes[len(th.Comments.Nodes)-1].Author.Login
		}
		// Actionable = a reviewer had the last word. Once the agent replies the
		// thread drops out of the actionable set (so pr-fix stops re-firing) but
		// stays unresolved (so the merge gate still holds until it's resolved).
		if lastAuthor != "" && !strings.EqualFold(lastAuthor, agentLogin) {
			actionable++
		}
		// The signature keys on the unresolved-thread set + review decision only,
		// not on who commented last or comment ids. So the agent's own replies
		// never change it — the retry budget caps honestly at MaxRetries —
		// while a new reviewer thread does change it (a fresh budget).
		sigTokens = append(sigTokens, th.ID)
	}
	feedbackSig := reviewFeedbackSig(n.ReviewDecision, sigTokens)

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
		BaseRefName:       n.BaseRefName,
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
		ActionableCount:   actionable,
		FeedbackSig:       feedbackSig,
		ViewerHasApproved: viewerApproved,
		CopilotReviewed:   copilotReviewed,
		SelfAuthoredBot:   isBot(n.Author.Type, n.Author.Login) && sameActor(n.Author.Login, viewer),
		CreatedAt:         n.CreatedAt,
		UpdatedAt:         n.UpdatedAt,
		AutoMergeEnabled:  n.AutoMergeRequest != nil,
	}
}

// reviewFeedbackSig fingerprints a PR's reviewer feedback so the pr-fix retry
// budget can tell genuinely-new feedback (reset the budget) from stale,
// already-addressed feedback (let the budget cap and escalate). The tokens are
// the unresolved-thread IDs; sorted and hashed with the review decision. Keying
// on the thread set (not comment ids or who replied last) means the agent's own
// replies never change the signature — a thread it replied to is still
// unresolved, so its ID stays in the set — while a new reviewer thread does.
// Returns "" when there is no feedback at all (no unresolved threads and no
// change request).
func reviewFeedbackSig(reviewDecision string, tokens []string) string {
	if reviewDecision != "CHANGES_REQUESTED" && len(tokens) == 0 {
		return ""
	}
	sort.Strings(tokens)
	h := sha256.New()
	h.Write([]byte(reviewDecision))
	for _, tok := range tokens {
		h.Write([]byte{'\n'})
		h.Write([]byte(tok))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func convertPRs(nodes []gqlPR, viewer string) []PullRequest {
	prs := make([]PullRequest, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if isBot(n.Author.Type, n.Author.Login) && !sameActor(n.Author.Login, viewer) {
			continue
		}
		prs = append(prs, convertCommonPR(n, viewer))
	}
	return prs
}

func isBot(typeName, login string) bool {
	return typeName == "Bot" || strings.Contains(login, "[bot]")
}

func sameActor(a, b string) bool {
	strip := func(s string) string {
		return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "[bot]")
	}
	base := strip(a)
	return base != "" && base == strip(b)
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

// ViewerLogin returns the login this process acts as — the authenticated user,
// or "<slug>[bot]" under GitHub App auth. Returns "" if it cannot be resolved.
func ViewerLogin() string {
	return ViewerLoginCtx(context.Background())
}

// ViewerLoginCtx is ViewerLogin bound to a caller's context.
func ViewerLoginCtx(ctx context.Context) string {
	return viewerLogin(ctx, defaultExecer)
}

// IsTransientError reports whether err is a transient GitHub API failure
// (HTTP 5xx, rate limiting, or a network blip) that is expected to resolve on
// its own.
//
// Callers escalate on the non-transient side, and these errors arrive on every
// in-flight request at once, so a gap here turns a ten-second network wobble
// into a board-wide escalation storm.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	// A skipped optional poll (low GraphQL budget) is transient by design: back
	// off and retry next cycle rather than treating it as a hard fetch failure.
	if errors.Is(err, ErrBudgetExhausted) {
		return true
	}
	msg := strings.ToLower(err.Error())
	// HTTP 5xx: sanitized gh output produces "gh: http 5xx"
	if strings.Contains(msg, "http 5") {
		return true
	}
	// Rate limiting is backpressure, not a defect — GitHub is telling us to wait.
	if isRateLimitedMessage(msg) {
		return true
	}
	return strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "tls handshake timeout") ||
		strings.Contains(msg, "no route to host")
}

// IsAuthError reports whether err is a GitHub authentication failure — an
// invalid/expired/revoked token (HTTP 401 / "Bad credentials") or gh having
// no credentials configured at all (its local preflight fails before any
// request with a "please run gh auth login" guidance message rather than an
// API error). Neither resolves on its own: an invalid token needs a human to
// rotate it, and a missing token needs App auth or `gh auth login`
// configured — so pollers should circuit-break on this instead of retrying
// at their normal cadence.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "bad credentials") ||
		strings.Contains(msg, "gh auth login") ||
		strings.Contains(msg, "gh_token environment variable")
}
