package monitor

import "strings"

// Fingerprint returns a stable dedup key for an anomaly. The key is reused on
// every cycle so the issue sink can collapse repeated drift into a single open
// issue (and the in-process cooldown can throttle re-submission).
//
// Per-task anomalies bind to the task id; board-wide anomalies bind to a
// secondary discriminator from Evidence (status name for bottleneck, severity
// for everything else) so independent failure modes don't collide.
func Fingerprint(kind AnomalyKind, taskID string, evidence map[string]any) string {
	if taskID != "" {
		return string(kind) + ":" + taskID
	}
	if status, ok := evidence["status"].(string); ok && status != "" {
		return string(kind) + ":" + status
	}
	return string(kind)
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
