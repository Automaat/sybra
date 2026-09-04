package monitor

import (
	"regexp"
	"strings"
)

// Fingerprint returns a stable dedup key for an anomaly. The key is reused on
// every cycle so the issue sink can collapse repeated drift into a single open
// issue (and the in-process cooldown can throttle re-submission).
//
// Per-task anomalies bind to the task id; board-wide anomalies bind to a
// secondary discriminator from Evidence (status name for bottleneck, severity
// for everything else) so independent failure modes don't collide.
//
// An optional evidence["cause"] appends a normalized suffix so two distinct
// failure modes on the same kind+task never collapse into one issue (e.g. a
// lost_agent remediation that errors gets its own fingerprint per distinct
// error, instead of reusing whatever issue an earlier, unrelated failure
// opened). Absent for every existing caller, so this is purely additive.
func Fingerprint(kind AnomalyKind, taskID string, evidence map[string]any) string {
	base := string(kind)
	switch {
	case taskID != "":
		base += ":" + taskID
	default:
		if status, ok := evidence["status"].(string); ok && status != "" {
			base += ":" + status
		}
	}
	if cause, ok := evidence["cause"].(string); ok {
		if slug := causeSlug(cause); slug != "" {
			base += ":" + slug
		}
	}
	return base
}

// causeNonAlnum matches runs of characters causeSlug collapses to a single
// "-" so a raw error message (which may contain quotes, colons, whitespace)
// turns into a title- and gh-search-safe token.
var causeNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// causeSlug normalizes a free-form cause string (typically an error message)
// into a short, stable token suitable for a fingerprint/title. Truncated so a
// verbose error doesn't blow out the "[monitor] ..." issue title.
func causeSlug(cause string) string {
	s := causeNonAlnum.ReplaceAllString(strings.ToLower(strings.TrimSpace(cause)), "-")
	s = strings.Trim(s, "-")
	const maxLen = 40
	if len(s) > maxLen {
		s = strings.Trim(s[:maxLen], "-")
	}
	return s
}

// IssueTitle builds a human-readable, fingerprint-stable issue title. The
// "[monitor] " prefix is matched by the dedup query in IssueSink.
func IssueTitle(kind AnomalyKind, fp string) string {
	short := strings.TrimPrefix(fp, string(kind)+":")
	if short == "" || short == fp {
		return "[monitor] " + string(kind)
	}
	return "[monitor] " + string(kind) + ": " + short
}
