package agent

import "testing"

// TestDiagnosesBlockedTaskCoversDetectorRoles pins, for every declared role,
// whether the status reapers may kill it. Both reapers stop agents whose task has reached
// human-required or a terminal status; these roles are dispatched *because*
// of that status, so reaping them leaves the task stuck and the detector
// re-dispatching next cycle — a livelock costing a full agent run per pass.
// That is exactly what happened to monitor:stuck_human_blocked agents, which
// were killed ~26s in, every cycle, without ever unblocking anything.
func TestDiagnosesBlockedTaskCoversDetectorRoles(t *testing.T) {
	t.Parallel()
	// Exhaustive over allRoles rather than a hand-picked sample: a new role
	// defaulting to reapable is fine, but it should be a decision someone
	// made, not one this test failed to notice.
	exempt := map[Role]string{
		RoleHumanReview: "dispatched on the human-required transition to unblock it",
		RoleMonitor:     "stuck_human_blocked detector acts on human-required tasks",
	}
	for _, r := range allRoles {
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			why, want := exempt[r]
			got := r.DiagnosesBlockedTask()
			if got == want {
				return
			}
			if want {
				t.Errorf("%s.DiagnosesBlockedTask() = false, want true (%s)", r, why)
				return
			}
			t.Errorf("%s.DiagnosesBlockedTask() = true, want false — a task-scoped worker must stop when its task is no longer actionable", r)
		})
	}
}
