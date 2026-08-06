package agent

import "testing"

// TestDiagnosesBlockedTaskCoversDetectorRoles pins the roles the status
// reapers must not kill. Both reapers stop agents whose task has reached
// human-required or a terminal status; these roles are dispatched *because*
// of that status, so reaping them leaves the task stuck and the detector
// re-dispatching next cycle — a livelock costing a full agent run per pass.
// That is exactly what happened to monitor:stuck_human_blocked agents, which
// were killed ~26s in, every cycle, without ever unblocking anything.
func TestDiagnosesBlockedTaskCoversDetectorRoles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role Role
		want bool
		why  string
	}{
		{role: RoleHumanReview, want: true, why: "dispatched on the human-required transition to unblock it"},
		{role: RoleMonitor, want: true, why: "stuck_human_blocked detector acts on human-required tasks"},

		{role: RoleImplementation, want: false, why: "must stop when its task is no longer actionable"},
		{role: RoleReview, want: false, why: "same"},
		{role: RoleTestRunner, want: false, why: "same"},
		{role: RolePlan, want: false, why: "same"},
		{role: RoleFixReview, want: false, why: "same"},
		{role: RolePRFix, want: false, why: "same"},
		{role: RoleOrchestrator, want: false, why: "not task-scoped"},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			t.Parallel()
			if got := tc.role.DiagnosesBlockedTask(); got != tc.want {
				t.Errorf("%s.DiagnosesBlockedTask() = %v, want %v (%s)", tc.role, got, tc.want, tc.why)
			}
		})
	}
}
