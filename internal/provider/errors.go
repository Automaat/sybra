// Package provider tracks CLI provider (claude, codex) auth and rate-limit
// health so the agent manager can gate scheduling and failover between
// providers when one becomes unusable.
package provider

import (
	"errors"
	"fmt"
	"time"
)

// ErrProviderUnhealthy sentinel — use errors.Is to detect gate-block errors.
var ErrProviderUnhealthy = errors.New("provider unhealthy")

// RateLimitReason is the Status.Reason value set when a provider is throttled
// (ReportRateLimit). Exposed so callers can tell a transient capacity throttle
// (self-heals when the cooldown window expires) apart from an auth failure
// (needs a human login) without string-matching a literal.
const RateLimitReason = "rate_limited"

// UnhealthyError carries the structured reason a provider was refused.
//
// RateLimited marks a transient, self-healing capacity throttle (a health-gate
// rate limit or a limits-store quota-reached block) as opposed to a permanent
// refusal (auth failure needing a human login, provider disabled in config).
// Downstream classification (workflow.isTransientCapacityError) keys off it so a
// rate limit parks-and-retries instead of feeding the dispatch circuit breaker.
// A rate limit is not always tagged with Until (the limits store knows the
// condition but not always the exact reset time), so the flag is the reliable
// signal where a zero Until would otherwise read as "not a rate limit".
type UnhealthyError struct {
	Provider    string
	Reason      string
	Until       time.Time
	RateLimited bool
}

func (e *UnhealthyError) Error() string {
	if e == nil {
		return "provider unhealthy"
	}
	if !e.Until.IsZero() {
		return fmt.Sprintf("provider %s unhealthy (%s) until %s", e.Provider, e.Reason, e.Until.Format(time.RFC3339))
	}
	return fmt.Sprintf("provider %s unhealthy (%s)", e.Provider, e.Reason)
}

func (e *UnhealthyError) Is(target error) bool {
	return target == ErrProviderUnhealthy
}
