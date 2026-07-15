package health

// FingerprintFor returns a stable dedup key for a finding. Per-task findings
// bind to the task id; board-wide findings bind to a secondary discriminator
// from Evidence (status name for bottlenecks) so independent failure modes do
// not collide.
//
// The hash algorithm is intentionally identical to monitor.Fingerprint at
// internal/monitor/fingerprint.go so the two packages produce the same keys
// for equivalent inputs. A parity test in fingerprint_test.go guards against
// drift.
func FingerprintFor(f *Finding) string {
	if f == nil {
		return ""
	}
	if f.TaskID != "" {
		return string(f.Category) + ":" + f.TaskID
	}
	if status, ok := f.Evidence["status"].(string); ok && status != "" {
		return string(f.Category) + ":" + status
	}
	return string(f.Category)
}
