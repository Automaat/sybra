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

// UnhealthyError carries the structured reason a provider was refused.
type UnhealthyError struct {
	Provider string
	Reason   string
	Until    time.Time
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
