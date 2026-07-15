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

func TestRole_DefaultReasoningEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role Role
		want string
	}{
		{RoleTriage, "low"},
		{RoleEval, "low"},
		{RolePlanCritic, "low"},
		{RoleHumanReview, "low"},
		{RoleImplementation, "high"},
		{RoleFixReview, "high"},
		{RolePRFix, "high"},
		{RolePlan, ""},
		{RoleReview, ""},
		{RoleTestRunner, ""},
		{Role(""), ""},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()
			got := tt.role.DefaultReasoningEffort()
			if got != tt.want {
				t.Errorf("DefaultReasoningEffort() = %q, want %q", got, tt.want)
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

func TestRole_SupportsHeadlessSteer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role Role
		want bool
	}{
		// Unattended verifier/system dispatch — never receives a steer message.
		{RoleReview, false},
		{RoleTestRunner, false},
		{RoleEval, false},
		{RoleTriage, false},
		{RolePlanCritic, false},
		{RoleHumanReview, false},
		{RoleFixReview, false},
		// Roles a human may actively watch and steer from the GUI.
		{RoleImplementation, true},
		{RolePlan, true},
		{RolePRFix, true},
		{Role(""), true}, // empty maps to implementation
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()
			if got := tt.role.SupportsHeadlessSteer(); got != tt.want {
				t.Errorf("Role(%q).SupportsHeadlessSteer() = %v, want %v", tt.role, got, tt.want)
			}
		})
	}
}

func TestRole_AuthorsCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role Role
		want bool
	}{
		// Code authors — primed with NOTES.md working memory.
		{RoleImplementation, true},
		{RoleFixReview, true},
		{RolePRFix, true},
		{Role(""), true}, // empty maps to implementation
		// Independent verifiers — must NOT inherit the implementer's scratchpad,
		// or the reward-hacking defense is silently weakened.
		{RoleReview, false},
		{RoleTestRunner, false},
		{RoleEval, false},
		{RolePlan, false},
		{RolePlanCritic, false},
		{RoleTriage, false},
		{RoleHumanReview, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()
			if got := tt.role.AuthorsCode(); got != tt.want {
				t.Errorf("Role(%q).AuthorsCode() = %v, want %v", tt.role, got, tt.want)
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

func TestParseRoleFromName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		want   Role
		wantOK bool
	}{
		{"test-runner:Run Tests", RoleTestRunner, true},
		{"implementation:Impl", RoleImplementation, true},
		{"unknown:something", RoleImplementation, false},
		{"no-colon", RoleImplementation, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseRoleFromName(tt.name)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ParseRoleFromName(%q) = (%q, %v), want (%q, %v)", tt.name, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
