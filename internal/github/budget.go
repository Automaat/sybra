package github

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// rateLimitResponse mirrors the GET /rate_limit body. The endpoint is free —
// it never counts against any quota — so polling it cheaply keeps the request
// gate's budget view accurate for every gh path, including the `gh pr view` /
// `gh issue` subcommands whose response headers the gate can't observe.
type rateLimitResponse struct {
	Resources map[string]struct {
		Limit     int   `json:"limit"`
		Remaining int   `json:"remaining"`
		Reset     int64 `json:"reset"`
	} `json:"resources"`
}

// RefreshRateBudget queries GET /rate_limit and feeds every resource bucket
// into the shared request gate. Safe to call on a timer; the endpoint is
// exempt from rate limiting.
func RefreshRateBudget(ctx context.Context) error {
	return refreshRateBudgetWith(ctx, defaultExecer)
}

func refreshRateBudgetWith(ctx context.Context, e execer) error {
	// No --cache: a stale budget snapshot defeats the purpose.
	resp, err := runGHAPICtxWith(ctx, e, "", "rate_limit")
	if err != nil {
		return fmt.Errorf("gh api rate_limit: %s: %w", sanitizeGHOutput(resp.body), err)
	}
	var parsed rateLimitResponse
	if err := json.Unmarshal(resp.body, &parsed); err != nil {
		return fmt.Errorf("parse rate_limit: %w", err)
	}
	for name, r := range parsed.Resources {
		var resetAt time.Time
		if r.Reset > 0 {
			resetAt = time.Unix(r.Reset, 0)
		}
		ghGate.setResource(name, r.Remaining, r.Limit, resetAt)
	}
	return nil
}

// BudgetPressureFactor returns a multiplier (>= 1) to stretch poll intervals
// when the GitHub rate budget runs low. It is 1 when budget is healthy or
// unknown, scaling up to 4x as the lowest resource bucket approaches
// exhaustion. Lets every poller back off in concert under a shared, possibly
// near-exhausted, token without each re-implementing the heuristic.
func BudgetPressureFactor() float64 {
	frac, known := ghGate.pressure()
	if !known {
		return 1
	}
	switch {
	case frac <= 0.05:
		return 4
	case frac <= 0.15:
		return 2
	case frac <= 0.30:
		return 1.5
	default:
		return 1
	}
}

// ScaleInterval stretches a poll interval by the current budget pressure so all
// pollers back off together when the shared token runs low.
func ScaleInterval(d time.Duration) time.Duration {
	f := BudgetPressureFactor()
	if f <= 1 {
		return d
	}
	return time.Duration(float64(d) * f)
}
