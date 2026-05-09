package github

import "strings"

// informationalCheckPrefixes lists check-name prefixes that report on
// codebase health (coverage, code-quality scans) but do not gate
// mergeability. We exclude them from `CIStatus` so a failing
// `codecov/patch` does not trigger the pr-fix loop — there is no code
// change a fixer agent can make to bump coverage from a formatting
// commit, so retrying just burns budget. Required-check enforcement
// belongs to GitHub branch protection, not to this client.
//
// Match is by lowercase prefix: `codecov/patch`, `codecov/project`,
// `sonarcloud/quality-gate`, etc. all match `codecov/` or `sonarcloud/`.
// Override from tests via the package-level slice; production code
// should not mutate it at runtime.
var informationalCheckPrefixes = []string{
	"codecov/",
	"sonarcloud/",
	"sonarsource/",
	"deepsource/",
}

// isInformationalCheck reports whether the check name belongs to a
// non-gating reporter (coverage, quality scan). Empty name returns
// false so unparseable contexts still feed the rollup.
func isInformationalCheck(name string) bool {
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	for _, p := range informationalCheckPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// effectiveCheckState normalizes a single context to one of
// SUCCESS/FAILURE/PENDING. Returns "" when the context is malformed
// (no usable typename and no state fields).
//
// CheckRun: status != COMPLETED → PENDING; conclusion is mapped to
// SUCCESS for benign outcomes (NEUTRAL, SKIPPED, CANCELLED, STALE) and
// FAILURE for blocking outcomes (FAILURE, TIMED_OUT, STARTUP_FAILURE,
// ACTION_REQUIRED). Unknown conclusions on a completed CheckRun fall
// through to SUCCESS — better to skip than to spam pr-fix on a check
// state we don't recognize.
//
// StatusContext: PENDING/EXPECTED → PENDING; ERROR/FAILURE → FAILURE;
// SUCCESS → SUCCESS.
func effectiveCheckState(c gqlCheckContext) string {
	switch c.Typename {
	case "CheckRun":
		if c.Status != "" && c.Status != "COMPLETED" {
			return "PENDING"
		}
		switch strings.ToUpper(c.Conclusion) {
		case "FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED":
			return "FAILURE"
		case "", "SUCCESS", "NEUTRAL", "SKIPPED", "CANCELLED", "STALE":
			return "SUCCESS"
		default:
			return "SUCCESS"
		}
	case "StatusContext":
		switch strings.ToUpper(c.State) {
		case "FAILURE", "ERROR":
			return "FAILURE"
		case "PENDING", "EXPECTED":
			return "PENDING"
		case "SUCCESS":
			return "SUCCESS"
		default:
			return ""
		}
	default:
		// Older gh / GraphQL shapes that don't include __typename: best
		// effort using whichever fields are populated.
		if c.Status != "" || c.Conclusion != "" {
			c.Typename = "CheckRun"
			return effectiveCheckState(c)
		}
		if c.State != "" {
			c.Typename = "StatusContext"
			return effectiveCheckState(c)
		}
		return ""
	}
}

// rollupFromContexts computes (ciStatus, hasPendingChecks) ignoring
// informational reporters. Returns ("", false) when contexts is empty
// so callers can fall back to the GitHub-supplied rollup state for
// PRs whose checks GraphQL didn't return.
//
// Precedence: any FAILURE → FAILURE; else any PENDING → PENDING; else
// SUCCESS. hasPendingChecks reflects pending state on the *filtered*
// set so MatchTaskPRs does not stall on a still-running codecov check.
func rollupFromContexts(contexts []gqlCheckContext) (ciStatus string, hasPendingChecks bool) {
	if len(contexts) == 0 {
		return "", false
	}

	var sawCounted, sawFailure, sawPending, sawSuccess bool
	for i := range contexts {
		if isInformationalCheck(contexts[i].Name) {
			continue
		}
		state := effectiveCheckState(contexts[i])
		if state == "" {
			continue
		}
		sawCounted = true
		switch state {
		case "FAILURE":
			sawFailure = true
		case "PENDING":
			sawPending = true
		case "SUCCESS":
			sawSuccess = true
		}
	}

	if !sawCounted {
		return "", false
	}
	switch {
	case sawFailure:
		return "FAILURE", sawPending
	case sawPending:
		return "PENDING", true
	case sawSuccess:
		return "SUCCESS", false
	default:
		return "", false
	}
}
