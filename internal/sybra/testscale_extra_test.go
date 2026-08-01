package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/testutil/loadscale"
)

// TestOversubscriptionFactorIgnoresIdleHost documents the property that made
// #2811 possible: the factor is derived from a one-minute load average, so a
// host that is about to become saturated still reports "idle" and yields no
// scaling. Waits therefore cannot treat the first measurement as final.
func TestOversubscriptionFactorIgnoresIdleHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		load float64
		ok   bool
		want int64
	}{
		{name: "idle host gets no scaling", load: 0.2, ok: true, want: 1},
		{name: "exactly one per cpu is not oversubscribed", load: 1, ok: true, want: 1},
		{name: "unreadable load fails safe to no scaling", load: 9, ok: false, want: 1},
		{name: "oversubscription rounds up", load: 3.2, ok: true, want: 4},
		{name: "ceiling caps the factor", load: 99, ok: true, want: 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := loadscale.OversubscriptionFactor(tc.load, tc.ok, 20); got != tc.want {
				t.Errorf("OversubscriptionFactor(%v, %v) = %d, want %d", tc.load, tc.ok, got, tc.want)
			}
		})
	}
}

// TestTimeoutScaleHonoursExplicitOverride pins the escape hatch the adaptive
// path relies on for testing, and that CI's floor cannot silently drop below
// itself.
func TestTimeoutScaleHonoursExplicitOverride(t *testing.T) {
	t.Setenv("SYBRA_E2E_TIMEOUT_SCALE", "7")
	if got := e2eTimeoutScale(); got != 7 {
		t.Fatalf("explicit override = %d, want 7", got)
	}
	t.Setenv("SYBRA_E2E_TIMEOUT_SCALE", "")
	t.Setenv("CI", "1")
	if got := e2eTimeoutScale(); got < e2eCITimeoutScaleFloor {
		t.Fatalf("CI scale = %d, want at least the floor %d", got, e2eCITimeoutScaleFloor)
	}
}
