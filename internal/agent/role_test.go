package agent

import "testing"

func TestRole_AgentName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role  Role
		title string
		want  string
	}{
		{RoleTriage, "My Task", "triage:My Task"},
		{RolePlan, "Plan Something", "plan:Plan Something"},
		{RolePlanCritic, "Critique Plan", "plan-critic:Critique Plan"},
		{RoleEval, "Evaluate", "eval:Evaluate"},
		{RolePRFix, "Fix PR", "pr-fix:Fix PR"},
		{RoleReview, "Review Code", "review:Review Code"},
		{RoleFixReview, "Fix Review", "fix-review:Fix Review"},
		{RoleTestPlan, "Plan Tests", "test-plan:Plan Tests"},
		{RoleTestRunner, "Run Tests", "test-runner:Run Tests"},
		{RoleImplementation, "Impl", "implementation:Impl"},
		{RoleTriage, "", "triage:"},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()
			got := tt.role.AgentName(tt.title)
			if got != tt.want {
				t.Errorf("AgentName(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

func TestRole_IsSystem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role Role
		want bool
	}{
		{RoleTriage, true},
		{RoleEval, true},
		{RolePlanCritic, true},
		{RolePlan, false},
		{RolePRFix, false},
		{RoleReview, false},
		{RoleFixReview, false},
		{RoleTestPlan, false},
		{RoleTestRunner, false},
		{RoleImplementation, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()
			got := tt.role.IsSystem()
			if got != tt.want {
				t.Errorf("IsSystem() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoleFromName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want Role
	}{
		{"triage:My Task", RoleTriage},
		{"plan:Plan Task", RolePlan},
		{"plan-critic:Critique Plan", RolePlanCritic},
		{"eval:Evaluate", RoleEval},
		{"pr-fix:Fix PR", RolePRFix},
		{"review:Code Review", RoleReview},
		{"fix-review:Fix Review", RoleFixReview},
		{"test-plan:Plan Tests", RoleTestPlan},
		{"test-runner:Run Tests", RoleTestRunner},
		{"implementation:Impl", RoleImplementation},
		{"unknown:something", RoleImplementation},
		{"no-colon", RoleImplementation},
		{"", RoleImplementation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := RoleFromName(tt.name)
			if got != tt.want {
				t.Errorf("RoleFromName(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
