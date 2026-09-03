package workflow

import (
	"testing"
	"time"
)

// TestDeadlineBeatLoad_SeparatesContentionFromSlowTests pins the distinction a
// fixed budget cannot make on its own.
//
// The budget is resolved before the run and never revisited, but the
// oversubscription it exists to absorb develops during it. A verify run that
// started on an idle host and finished on one carrying several agents kept the
// idle budget and reported "fix slow or hanging tests" — sending an operator
// after a defect in code that had merely been descheduled.
func TestDeadlineBeatLoad_SeparatesContentionFromSlowTests(t *testing.T) {
	cases := []struct {
		name        string
		loadNow     float64
		loadOK      bool
		base        time.Duration
		spent       time.Duration
		wantScaled  time.Duration
		wantStarved bool
	}{
		{
			// The host filled up while the suite ran: at the load measured now
			// the same run would have had 30m, so 10m was never the real budget.
			name:    "host oversubscribed by the time the deadline fired",
			loadNow: 3, loadOK: true,
			base: 10 * time.Minute, spent: 10 * time.Minute,
			wantScaled: 30 * time.Minute, wantStarved: true,
		},
		{
			// Idle host: the budget it got is the budget it deserved, so the
			// suite really is too slow and must be reported as such.
			name:    "host quiet, the suite is genuinely over budget",
			loadNow: 0.5, loadOK: true,
			base: 10 * time.Minute, spent: 10 * time.Minute,
			wantScaled: 10 * time.Minute, wantStarved: false,
		},
		{
			// No load signal is not evidence of contention.
			name:    "load unreadable",
			loadNow: 0, loadOK: false,
			base: 10 * time.Minute, spent: 10 * time.Minute,
			wantScaled: 10 * time.Minute, wantStarved: false,
		},
		{
			// Scaling is capped, so a runaway reading cannot excuse a real hang
			// indefinitely.
			name:    "load past the ceiling still caps",
			loadNow: 500, loadOK: true,
			base: time.Minute, spent: time.Duration(verifyTimeoutScaleCeiling) * time.Minute,
			wantScaled: time.Duration(verifyTimeoutScaleCeiling) * time.Minute, wantStarved: false,
		},
		{
			// An unset base resolves to the compiled default before scaling.
			name:    "unset base uses the default",
			loadNow: 2, loadOK: true,
			base: 0, spent: verifyChecksDefaultTimeout,
			wantScaled: 2 * verifyChecksDefaultTimeout, wantStarved: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := workflowCheckLoadPerCPU
			workflowCheckLoadPerCPU = func() (float64, bool) { return tc.loadNow, tc.loadOK }
			t.Cleanup(func() { workflowCheckLoadPerCPU = orig })

			scaled, starved := deadlineBeatLoad(tc.base, tc.spent)
			if scaled != tc.wantScaled {
				t.Errorf("scaled = %s, want %s", scaled, tc.wantScaled)
			}
			if starved != tc.wantStarved {
				t.Errorf("starved = %v, want %v", starved, tc.wantStarved)
			}
		})
	}
}
