package agent

import (
	"slices"
	"strings"
	"testing"
)

// deniedToolsArg renders the denial list the way it appears on the command
// line, so assertions track the list instead of restating it — a restated
// copy would have to be edited in lockstep and would drift.
func deniedToolsArg() string { return strings.Join(headlessDeniedTools, ",") }

// TestHeadlessDeniedToolsDenyRatherThanAllow pins the choice of lever. A
// non-empty --allowedTools outranks agent.require_permissions entirely, so
// shrinking the surface with an allowlist would switch the permission layer
// off as a side effect. Denial composes with every permission posture.
func TestHeadlessDeniedToolsDenyRatherThanAllow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		allowed      []string
		requirePerms bool
		mode         string
	}{
		{name: "skip-permissions posture"},
		{name: "approval-hook posture", requirePerms: true},
		{name: "auto classifier posture", mode: "auto"},
		{name: "explicit allowlist posture", allowed: []string{"Read", "Bash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := claudePermissionArgs(tc.allowed, tc.requirePerms, tc.mode)
			i := slices.Index(args, "--disallowedTools")
			if i < 0 || i+1 >= len(args) {
				t.Fatalf("args = %v, want a --disallowedTools flag in every posture", args)
			}
			// Named explicitly rather than ranged over headlessDeniedTools:
			// iterating the production list makes the assertion shrink with
			// it, so dropping an entry would pass. Each of these has a stated
			// reason on the list; removing one should be a deliberate edit in
			// two places.
			denied := strings.Split(args[i+1], ",")
			for _, tool := range []string{
				"ScheduleWakeup",
				"CronCreate", "CronDelete", "CronList",
				"EnterWorktree", "ExitWorktree",
				"TaskGet", "TaskList",
				"NotebookEdit",
				"PushNotification", "RemoteTrigger", "DesignSync",
			} {
				if !slices.Contains(denied, tool) {
					t.Errorf("%q missing from denied list %q", tool, args[i+1])
				}
			}
		})
	}
}

// TestHeadlessDeniedToolsKeepResearchTools guards against over-cutting. These
// went unused in the sampled window, but absence of use over one window is not
// evidence a capability is unwanted — and that sample was dominated by a
// single role.
func TestHeadlessDeniedToolsKeepResearchTools(t *testing.T) {
	t.Parallel()
	for _, keep := range []string{"WebFetch", "WebSearch", "Workflow", "Read", "Bash", "Edit", "Write"} {
		if slices.Contains(headlessDeniedTools, keep) {
			t.Errorf("%q is denied; it is either load-bearing or merely unused, neither of which justifies removing it", keep)
		}
	}
}
