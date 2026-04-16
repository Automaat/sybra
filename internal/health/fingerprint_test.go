package health

import (
	"testing"

	"github.com/Automaat/sybra/internal/monitor"
)

func TestFingerprintFor(t *testing.T) {
	tests := []struct {
		name    string
		finding Finding
		want    string
	}{
		{
			name:    "per-task finding",
			finding: Finding{Category: CatStuckTask, TaskID: "task-abc"},
			want:    "stuck_task:task-abc",
		},
		{
			name:    "board-wide with status discriminator",
			finding: Finding{Category: CatStatusBottleneck, Evidence: map[string]any{"status": "in-review"}},
			want:    "status_bottleneck:in-review",
		},
		{
			name:    "board-wide without discriminator",
			finding: Finding{Category: CatFailureRate, Evidence: map[string]any{"failure_rate": 0.5}},
			want:    "failure_rate",
		},
		{
			name:    "task id wins over evidence status",
			finding: Finding{Category: CatStuckTask, TaskID: "task-xyz", Evidence: map[string]any{"status": "todo"}},
			want:    "stuck_task:task-xyz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FingerprintFor(&tt.finding)
			if got != tt.want {
				t.Errorf("FingerprintFor = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFingerprintParity asserts that health.FingerprintFor produces the same
// keys as monitor.Fingerprint for equivalent inputs. The two packages must
// stay in lockstep because selfmonitor joins against issues filed by both.
func TestFingerprintParity(t *testing.T) {
	type row struct {
		kind     string
		taskID   string
		evidence map[string]any
	}
	rows := []row{
		{"stuck_task", "task-1", nil},
		{"status_bottleneck", "", map[string]any{"status": "in-review"}},
		{"failure_rate", "", map[string]any{"failure_rate": 0.42}},
		{"cost_outlier", "task-2", map[string]any{"role": "eval"}},
		{"cost_drift", "", nil},
	}
	for _, r := range rows {
		f := Finding{Category: Category(r.kind), TaskID: r.taskID, Evidence: r.evidence}
		got := FingerprintFor(&f)
		want := monitor.Fingerprint(monitor.AnomalyKind(r.kind), r.taskID, r.evidence)
		if got != want {
			t.Errorf("parity mismatch for %+v: health=%q monitor=%q", r, got, want)
		}
	}
}
