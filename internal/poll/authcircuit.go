package poll

import (
	"log/slog"
	"sync"
	"time"
)

// AuthFailureThreshold is the number of consecutive GitHub auth failures
// (401 / "Bad credentials") a poller tolerates before it circuit-breaks.
// A dead server token is not transient — retrying every cycle just
// re-hammers a doomed request and floods the log (#1516: ~32k "Bad
// credentials" warns over 7 weeks from one stale token, ~860/day, zero
// useful work).
const AuthFailureThreshold = 5

// AuthCircuitBackoff is the poll interval a Fetcher should return while its
// AuthCircuit is open.
const AuthCircuitBackoff = 30 * time.Minute

// AuthCircuit tracks consecutive GitHub auth failures for one poller and
// reports whether it should be considered CRITICAL. Once open, callers
// should back off to AuthCircuitBackoff and stop warn-logging every cycle —
// RecordFailure only logs at the moment the breaker trips.
type AuthCircuit struct {
	name   string
	logger *slog.Logger

	mu          sync.Mutex
	consecutive int
	open        bool
}

// NewAuthCircuit returns a breaker for a poller identified by name (used in
// log lines and the exported poller-health gauge).
func NewAuthCircuit(name string, logger *slog.Logger) *AuthCircuit {
	return &AuthCircuit{name: name, logger: logger}
}

// RecordFailure records one consecutive GitHub auth failure. Logs once, at
// ERROR, the moment the breaker crosses AuthFailureThreshold; stays silent
// on every subsequent failure while already open. A nil receiver (a Handler
// built via a bare struct literal, as many tests do, instead of its
// constructor) is a no-op — the breaker simply never trips.
func (c *AuthCircuit) RecordFailure(err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutive++
	if !c.open && c.consecutive >= AuthFailureThreshold {
		c.open = true
		c.logger.Error("poller.auth_circuit_open", "poller", c.name, "consecutive", c.consecutive, "err", err)
	}
}

// RecordSuccess resets the breaker. Logs a recovery line if it had been open.
func (c *AuthCircuit) RecordSuccess() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	wasOpen := c.open
	c.consecutive = 0
	c.open = false
	if wasOpen {
		c.logger.Info("poller.auth_circuit_closed", "poller", c.name)
	}
}

// Open reports whether the breaker is currently tripped (poller CRITICAL).
// A nil receiver reports closed.
func (c *AuthCircuit) Open() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open
}
