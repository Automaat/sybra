package github

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ghRequestSpacing      = 150 * time.Millisecond
	ghLowBudgetThreshold  = 150
	ghOptionalCooldown    = 30 * time.Second
	ghFallbackRateBackoff = 1 * time.Minute
	ghMaxRetries          = 2
	ghRetryBaseBackoff    = 500 * time.Millisecond

	// ghDiscoveryReserveFraction is the fraction of a resource bucket's limit
	// reserved for merge-path polls once budget runs low. Discovery polls
	// (issue fetch, renovate, review-request search) skip once remaining
	// budget falls to this fraction, keeping the band between it and
	// ghLowBudgetThreshold exclusively for merge-path GraphQL.
	ghDiscoveryReserveFraction = 0.2
)

// pollPriority distinguishes merge-path advancement (never skip until the
// hard floor) from discovery polls (skip earlier to reserve budget).
type pollPriority int

const (
	priorityMergePath pollPriority = iota
	priorityDiscovery
)

// ghRetrySleep waits before a transient-error retry (backoff grows with the
// attempt). A package var so tests can stub out the real delay.
var ghRetrySleep = func(attempt int) {
	time.Sleep(time.Duration(attempt+1) * ghRetryBaseBackoff)
}

// isTransientGHError reports whether a gh api failure looks like a transient
// gateway/network blip worth retrying — a 502/503/504 gateway response, a
// connection timeout, or a dropped stream — as opposed to a 4xx/auth error, a
// plain 500 (often a real server-side bug, not transient), or a rate limit.
// Rate limits are deliberately excluded: the request gate already paces those
// via notBefore backoff, and retrying them immediately would only make things
// worse.
func isTransientGHError(out []byte, err error) bool {
	if err == nil || isRateLimitedMessage(string(out)) {
		return false
	}
	msg := strings.ToLower(string(out))
	for _, sig := range []string{
		"http 502", "http 503", "http 504",
		"operation timed out", "i/o timeout", "deadline exceeded",
		"connection reset", "connection refused",
		"stream error", "unexpected eof", "tls handshake",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

type ttlCache[T any] struct {
	mu         sync.RWMutex
	items      map[string]ttlCacheEntry[T]
	maxEntries int
}

type ttlCacheEntry[T any] struct {
	value     T
	expiresAt time.Time
}

func newTTLCache[T any]() *ttlCache[T] {
	return &ttlCache[T]{items: make(map[string]ttlCacheEntry[T])}
}

func newBoundedTTLCache[T any](maxEntries int) *ttlCache[T] {
	return &ttlCache[T]{items: make(map[string]ttlCacheEntry[T]), maxEntries: maxEntries}
}

func (c *ttlCache[T]) Get(key string) (T, bool) {
	var zero T
	c.mu.RLock()
	entry, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return zero, false
	}
	if !entry.expiresAt.IsZero() && time.Now().After(entry.expiresAt) {
		c.mu.Lock()
		delete(c.items, key)
		c.mu.Unlock()
		return zero, false
	}
	return entry.value, true
}

func (c *ttlCache[T]) GetStale(key string) (T, bool) {
	var zero T
	c.mu.RLock()
	entry, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return zero, false
	}
	return entry.value, true
}

func (c *ttlCache[T]) Set(key string, value T, ttl time.Duration) {
	now := time.Now()
	c.mu.Lock()
	c.pruneExpiredLocked(now)
	c.items[key] = ttlCacheEntry[T]{
		value:     value,
		expiresAt: now.Add(ttl),
	}
	c.evictOverflowLocked()
	c.mu.Unlock()
}

func (c *ttlCache[T]) pruneExpiredLocked(now time.Time) {
	for key, entry := range c.items {
		if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
			delete(c.items, key)
		}
	}
}

func (c *ttlCache[T]) evictOverflowLocked() {
	for c.maxEntries > 0 && len(c.items) > c.maxEntries {
		var oldestKey string
		var oldest time.Time
		first := true
		for key, entry := range c.items {
			if first || entry.expiresAt.Before(oldest) {
				oldestKey = key
				oldest = entry.expiresAt
				first = false
			}
		}
		delete(c.items, oldestKey)
	}
}

func (c *ttlCache[T]) Delete(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

type ghHTTPResponse struct {
	body       []byte
	headers    map[string]string
	statusCode int
}

func parseGHHTTPResponse(out []byte) ghHTTPResponse {
	raw := strings.ReplaceAll(string(out), "\r\n", "\n")
	if !strings.HasPrefix(raw, "HTTP/") {
		return ghHTTPResponse{body: out}
	}

	head, body, ok := strings.Cut(raw, "\n\n")
	if !ok {
		return ghHTTPResponse{body: out}
	}
	lines := strings.Split(head, "\n")
	resp := ghHTTPResponse{
		body:    []byte(body),
		headers: make(map[string]string),
	}

	if len(lines) > 0 {
		parts := strings.Fields(lines[0])
		if len(parts) >= 2 {
			if code, err := strconv.Atoi(parts[1]); err == nil {
				resp.statusCode = code
			}
		}
	}

	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		resp.headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}

	return resp
}

type ghRateWindow struct {
	remaining int
	limit     int
	resetAt   time.Time
}

type ghRequestGate struct {
	mu        sync.Mutex
	notBefore time.Time
	lastRun   time.Time
	resources map[string]ghRateWindow
}

func newGHRequestGate() *ghRequestGate {
	return &ghRequestGate{
		resources: make(map[string]ghRateWindow),
	}
}

func (g *ghRequestGate) execute(run func() ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	waitUntil := g.notBefore
	if next := g.lastRun.Add(ghRequestSpacing); next.After(waitUntil) {
		waitUntil = next
	}
	if sleep := time.Until(waitUntil); sleep > 0 {
		time.Sleep(sleep)
	}

	out, err := run()
	g.lastRun = time.Now()
	if err != nil && isRateLimitedMessage(string(out)) {
		g.bumpLocked(g.lastRun.Add(ghFallbackRateBackoff))
	}
	return out, err
}

func (g *ghRequestGate) observe(resp ghHTTPResponse, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now()
	if retryAfter, ok := resp.headers["retry-after"]; ok {
		if seconds, convErr := strconv.Atoi(strings.TrimSpace(retryAfter)); convErr == nil && seconds > 0 {
			g.bumpLocked(now.Add(time.Duration(seconds) * time.Second))
		}
	}

	resource := strings.ToLower(strings.TrimSpace(resp.headers["x-ratelimit-resource"]))
	if resource != "" {
		window := ghRateWindow{remaining: -1}
		if remaining, ok := resp.headers["x-ratelimit-remaining"]; ok {
			if n, convErr := strconv.Atoi(strings.TrimSpace(remaining)); convErr == nil {
				window.remaining = n
			}
		}
		if limit, ok := resp.headers["x-ratelimit-limit"]; ok {
			if n, convErr := strconv.Atoi(strings.TrimSpace(limit)); convErr == nil {
				window.limit = n
			}
		}
		if reset, ok := resp.headers["x-ratelimit-reset"]; ok {
			if ts, convErr := strconv.ParseInt(strings.TrimSpace(reset), 10, 64); convErr == nil && ts > 0 {
				window.resetAt = time.Unix(ts, 0)
			}
		}
		g.resources[resource] = window
		if window.remaining == 0 && !window.resetAt.IsZero() {
			g.bumpLocked(window.resetAt)
		}
	}

	if (err != nil || isGraphQLRateLimitBody(resp.body)) && isRateLimitedMessage(string(resp.body)) {
		g.bumpLocked(now.Add(ghFallbackRateBackoff))
	}
}

func (g *ghRequestGate) shouldSkipOptional(resource string, prio pollPriority) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	// The notBefore hard wall (secondary rate limit / bumped backoff) skips
	// every tier — there is no budget to reserve for merge-path when the API
	// itself has told us to back off entirely.
	if time.Until(g.notBefore) > 0 {
		return true
	}

	window, ok := g.resources[strings.ToLower(resource)]
	if !ok || window.remaining < 0 || window.resetAt.IsZero() {
		return false
	}

	threshold := ghLowBudgetThreshold
	if prio == priorityDiscovery && window.limit > 0 {
		if reserved := int(ghDiscoveryReserveFraction * float64(window.limit)); reserved > threshold {
			threshold = reserved
		}
	}

	if window.remaining > threshold {
		return false
	}
	return time.Until(window.resetAt) > ghOptionalCooldown
}

func (g *ghRequestGate) bumpLocked(until time.Time) {
	if until.After(g.notBefore) {
		g.notBefore = until
	}
}

// setResource records a rate-limit window for a resource bucket. Used by the
// /rate_limit refresher to give the gate budget visibility for every gh path,
// not only `gh api --include` calls whose response headers it can observe.
func (g *ghRequestGate) setResource(resource string, remaining, limit int, resetAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resources[strings.ToLower(strings.TrimSpace(resource))] = ghRateWindow{
		remaining: remaining,
		limit:     limit,
		resetAt:   resetAt,
	}
}

// lowestBudgetFraction returns the smallest remaining/limit ratio across the
// known resource buckets, plus whether any bucket is known. Used to scale poll
// intervals globally when budget runs low. A bucket with no recorded limit is
// skipped.
func (g *ghRequestGate) pressure() (fraction float64, known bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Until(g.notBefore) > 0 {
		return 0, true
	}
	minFrac := 1.0
	for _, w := range g.resources {
		if w.remaining < 0 || w.limit <= 0 {
			continue
		}
		f := float64(w.remaining) / float64(w.limit)
		if f < minFrac {
			minFrac = f
		}
		known = true
	}
	return minFrac, known
}

func isRateLimitedMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "secondary rate limit") ||
		strings.Contains(lower, "api rate limit exceeded") ||
		strings.Contains(lower, "rate limit exceeded") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "retry after")
}

func isGraphQLRateLimitBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, `"errors"`) && strings.Contains(lower, "rate")
}

func runGHAPIWith(e execer, cacheTTL string, args ...string) (ghHTTPResponse, error) {
	return runGHAPICtxWith(context.Background(), e, cacheTTL, args...)
}

// runGHAPICtxWith is runGHAPIWith with a context: a cancelled/expired context
// kills the in-flight gh process (when e supports it) and aborts the retry
// loop, so a stalled call cannot wedge the caller (e.g. the poll loop).
func runGHAPICtxWith(ctx context.Context, e execer, cacheTTL string, args ...string) (ghHTTPResponse, error) {
	cmdArgs := make([]string, 0, len(args)+4)
	cmdArgs = append(cmdArgs, "api")
	if cacheTTL != "" {
		cmdArgs = append(cmdArgs, "--cache", cacheTTL)
	}
	cmdArgs = append(cmdArgs, "--include")
	cmdArgs = append(cmdArgs, args...)

	// Retry transient gateway/network failures (502/503/504, timeouts,
	// dropped streams), but only for reads: a write (--method
	// POST/PUT/PATCH/DELETE) that returned a transient error may already have
	// applied server-side, so retrying it could double-act.
	write := isWriteRequest(args)
	var resp ghHTTPResponse
	var err error
	for attempt := 0; ; attempt++ {
		var out []byte
		out, err = runE(ctx, e, cmdArgs...)
		resp = parseGHHTTPResponse(out)
		ghGate.observe(resp, err)
		if err == nil || write || attempt >= ghMaxRetries || !isTransientGHError(out, err) {
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
			break
		}
		ghRetrySleep(attempt)
	}
	return resp, err
}

// isWriteRequest reports whether the gh api args specify a mutating HTTP
// method, so the retry path can leave writes alone.
func isWriteRequest(args []string) bool {
	for i, a := range args {
		if (a == "--method" || a == "-X") && i+1 < len(args) {
			switch strings.ToUpper(args[i+1]) {
			case "POST", "PUT", "PATCH", "DELETE":
				return true
			}
		}
	}
	return false
}

func runtimeCacheEnabled(e execer) bool {
	return e == defaultExecer
}

func prCacheKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

func invalidatePRCaches(repo string, number int) {
	key := prCacheKey(repo, number)
	prCache.Delete(key)
	prStatsCache.Delete(key)
	prStateCache.Delete(key)
	prFilesCache.Delete(key)
	prBranchCache.Delete(key)
	prHeadSHACache.Delete(key)
	prBaseSHACache.Delete(key)
	prContextCache.Delete(key)
	prClosingIssuesCache.Delete(key)
	pendingReviewCache.Delete(key)
	myReviewStateCache.Delete(key)
}

var (
	ghGate = newGHRequestGate()

	reviewSummaryCache   = newTTLCache[ReviewSummary]()
	assignedIssuesCache  = newTTLCache[[]Issue]()
	labeledIssuesCache   = newTTLCache[[]Issue]()
	renovatePRsCache     = newTTLCache[[]RenovatePR]()
	prCache              = newTTLCache[PullRequest]()
	prStatsCache         = newTTLCache[PRStats]()
	prStateCache         = newTTLCache[PRState]()
	prFilesCache         = newTTLCache[[]string]()
	prBranchCache        = newTTLCache[string]()
	prHeadSHACache       = newTTLCache[string]()
	prBaseSHACache       = newTTLCache[string]()
	commitParentsCache   = newBoundedTTLCache[[]string](512)
	prContextCache       = newTTLCache[PRContext]()
	prClosingIssuesCache = newTTLCache[prClosingIssuesResult]()
	pendingReviewCache   = newTTLCache[bool]()
	myReviewStateCache   = newTTLCache[MyReviewState]()
	issueCache           = newTTLCache[Issue]()
)

type prClosingIssuesResult struct {
	issues []int
	body   string
}
