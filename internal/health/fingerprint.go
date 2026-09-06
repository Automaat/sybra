package health

import (
	"maps"

	"github.com/Automaat/sybra/internal/monitor"
)

// FingerprintFor returns a stable dedup key for a finding. Per-task findings
// bind to the task id; board-wide findings bind to a secondary discriminator
// from Evidence (status name for bottlenecks) so independent failure modes do
// not collide.
//
// Delegating to monitor.Fingerprint keeps cause normalization identical for
// equivalent inputs. A structured Cause takes precedence over legacy evidence
// without mutating the finding's evidence map.
func FingerprintFor(f *Finding) string {
	if f == nil {
		return ""
	}
	evidence := f.Evidence
	if f.Cause != "" {
		evidence = maps.Clone(evidence)
		if evidence == nil {
			evidence = make(map[string]any)
		}
		evidence["cause"] = string(f.Cause)
	}
	return monitor.Fingerprint(monitor.AnomalyKind(f.Category), f.TaskID, evidence)
}
