package prompteval

import (
	"errors"
	"strings"

	"github.com/Automaat/sybra/internal/config"
)

// Gate decides whether a variant may be enrolled in online A/B tests, based
// on a precomputed verdict. It never runs a runner itself — Decision/
// AllowEnrollment only read the store, so they are safe to call on a
// dispatch hot path.
//
// Trust boundary: the digest is the identity key. A stale digest argument
// simply misses the stored verdict (or hits its own unrelated one) — it can
// never surface a verdict computed for different prompt bytes as a match.
type Gate struct {
	store *Store
	cfg   config.OfflineEvalConfig
}

// NewGate constructs a Gate reading verdicts from store.
func NewGate(store *Store, cfg config.OfflineEvalConfig) *Gate {
	return &Gate{store: store, cfg: cfg}
}

// Decision returns the stored verdict for (variantID, digest), or
// ErrNotFound if none exists.
func (g *Gate) Decision(variantID, digest string) (VariantVerdict, error) {
	return g.store.Read(variantID, digest)
}

// AllowEnrollment reports whether the variant may be enrolled online: FAIL
// always blocks, PASS always allows, and a missing/UNAVAILABLE verdict
// follows cfg.UnavailablePolicy (default "fail" — fail-closed).
func (g *Gate) AllowEnrollment(variantID, digest string) (allow bool, reason string, err error) {
	v, err := g.store.Read(variantID, digest)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return g.unavailableAllowed(), "no offline eval verdict recorded", nil
		}
		return false, "", err
	}
	switch v.Status {
	case StatusPass:
		return true, "", nil
	case StatusFail:
		return false, "offline eval failed: " + v.Reason, nil
	case StatusUnavailable:
		return g.unavailableAllowed(), "offline eval unavailable: " + v.Reason, nil
	default:
		return false, "unknown offline eval status", nil
	}
}

// unavailableAllowed resolves cfg.UnavailablePolicy. Default and any
// unrecognized value is fail-closed.
func (g *Gate) unavailableAllowed() bool {
	return strings.EqualFold(strings.TrimSpace(g.cfg.UnavailablePolicy), "pass")
}
