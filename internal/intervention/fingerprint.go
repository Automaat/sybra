package intervention

import "strings"

// Fingerprint returns a stable dedup key for rec: repeats of the same
// autonomy failure — same blocker kind, same operator response, same
// normalized blocker code, same status transition — collapse into one
// record with an incremented Recurrences instead of one record per
// occurrence. Deliberately excludes free-text fields (TaskID, OperatorReason,
// AttemptedActions, WorkflowStep) so two operators resolving the same class
// of failure on different tasks still aggregate.
//
// Styled after health.FingerprintFor/monitor.Fingerprint
// (internal/health/fingerprint.go, internal/monitor/fingerprint.go): a
// colon-joined key over the fields that define "the same failure mode",
// nothing that varies per-occurrence.
func Fingerprint(rec Record) string {
	parts := []string{
		rec.BlockerKind,
		string(rec.OperatorActionClass),
		normalizeCode(rec.BlockerCode),
		rec.FromStatus + "->" + rec.ToStatus,
	}
	return strings.Join(parts, ":")
}

// normalizeCode lowercases/trims BlockerCode so equivalent codes that differ
// only in casing or incidental whitespace still collapse to one fingerprint.
// BlockerCode is always a short code-authored token (see blocker.State.Code
// call sites), never free-form text, so no further slugging is needed.
func normalizeCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return "-"
	}
	return code
}
