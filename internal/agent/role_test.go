package agent

import (
	"testing"

	"github.com/Automaat/sybra/internal/roleeffort"
)

func TestRole_AgentName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role  Role
		title string
		want  string
	}{
		{RoleMonitor, "Investigate", "monitor:Investigate"},
		{RoleTriage, "My Task", "triage:My Task"},
		{RoleOrchestrator, "Brain", "orchestrator:Brain"},
		{RolePlan, "Plan Something", "plan:Plan Something"},
		{RolePlanCritic, "Critique Plan", "plan-critic:Critique Plan"},
		{RoleEval, "Evaluate", "eval:Evaluate"},
		{RolePRFix, "Fix PR", "pr-fix:Fix PR"},
		{RoleTestFix, "Fix Test", "test-fix:Fix Test"},
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
		{RoleMonitor, "low"},
		{RolePlanCritic, "low"},
		{RoleHumanReview, "low"},
		{RolePlan, "high"},
		{RoleReview, "high"},
		{RoleFixReview, "high"},
		{RolePRFix, "high"},
		{RoleTestFix, "high"},
		{RoleImplementation, ""},
		{RoleTestRunner, ""},
		{RoleLoop, ""},
		{RoleOrchestrator, ""},
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

// TestRole_DefaultReasoningEffort_CoversEveryKnownRole guards internal/roleeffort's
// plain-string switch against drift. It iterates AllRoles rather than a
// hand-maintained list, so a newly added Role fails here until someone decides
// its effort baseline, and a typo'd name in roleeffort's switch shows up as the
// wrong level instead of silently falling back to the global default.
func TestRole_DefaultReasoningEffort_CoversEveryKnownRole(t *testing.T) {
	t.Parallel()
	covered := map[Role]string{
		RoleTriage: "low", RoleEval: "low", RoleMonitor: "low",
		RolePlanCritic: "low", RoleHumanReview: "low",
		RolePlan: "high", RoleReview: "high", RoleFixReview: "high",
		RolePRFix: "high", RoleTestFix: "high",
		RoleImplementation: "", RoleTestRunner: "", RoleLoop: "", RoleOrchestrator: "",
	}
	all := AllRoles()
	for _, r := range all {
		want, ok := covered[r]
		if !ok {
			t.Errorf("role %q has no expected reasoning effort; add it to the table", r)
			continue
		}
		if got := r.DefaultReasoningEffort(); got != want {
			t.Errorf("DefaultReasoningEffort(%q) = %q, want %q", r, got, want)
		}
	}
	if len(all) != len(covered) {
		t.Errorf("AllRoles has %d roles, expectations have %d", len(all), len(covered))
	}
}

// TestDefaultReasoningEffort_MatchesRoleEffortGlobal pins the global fallback
// to the copy internal/abtest resolves omitted variant efforts against. The two
// packages cannot import each other, so nothing but this would catch a drift.
func TestDefaultReasoningEffort_MatchesRoleEffortGlobal(t *testing.T) {
	t.Parallel()
	if DefaultReasoningEffort != roleeffort.Global {
		t.Fatalf("DefaultReasoningEffort = %q, roleeffort.Global = %q; they must not drift",
			DefaultReasoningEffort, roleeffort.Global)
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
		{RoleLoop, true},
		{RoleMonitor, true},
		{RoleOrchestrator, true},
		{RolePlanCritic, true},
		{RolePlan, false},
		{RolePRFix, false},
		{RoleTestFix, false},
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
		{RoleLoop, false},
		{RoleMonitor, false},
		{RoleFixReview, false},
		// Roles a human may actively watch and steer from the GUI.
		{RoleImplementation, true},
		{RolePlan, true},
		{RolePRFix, true},
		{RoleTestFix, true},
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
		{RoleTestFix, true},
		{RoleHumanReview, true},
		{Role(""), true}, // empty maps to implementation
		// Independent verifiers — must NOT inherit the implementer's scratchpad,
		// or the reward-hacking defense is silently weakened.
		{RoleReview, false},
		{RoleTestRunner, false},
		{RoleEval, false},
		{RolePlan, false},
		{RolePlanCritic, false},
		{RoleTriage, false},
		{RoleLoop, false},
		{RoleMonitor, false},
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
		{"monitor:lost_agent", RoleMonitor},
		{"loop:self-monitor", RoleLoop},
		{"orchestrator:brain", RoleOrchestrator},
		{"pr-fix:Fix PR", RolePRFix},
		{"test-fix:Fix Test", RoleTestFix},
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
		{"monitor:lost_agent", RoleMonitor, true},
		{"loop:self-monitor", RoleLoop, true},
		{"pr-fix:Fix PR", RolePRFix, true},
		{"test-fix:Fix Test", RoleTestFix, true},
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

func TestResolveRunRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		role    Role
		agent   string
		want    Role
		wantErr bool
	}{
		{name: "explicit role wins", role: RoleMonitor, agent: "monitor:lost_agent", want: RoleMonitor},
		{name: "plain name defaults to implementation", agent: "Task title", want: RoleImplementation},
		{name: "known prefix is accepted", agent: "monitor:lost_agent", want: RoleMonitor},
		{name: "human title with colon defaults to implementation", agent: "refactor(agent): classify monitor runs", want: RoleImplementation},
		{name: "unknown prefixed name fails", agent: "future:thing", wantErr: true},
		{name: "unknown explicit role fails", role: Role("future"), agent: "Task title", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveRunRole(tt.role, tt.agent)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ResolveRunRole error = nil, want non-nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRunRole error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveRunRole(%q, %q) = %q, want %q", tt.role, tt.agent, got, tt.want)
			}
		})
	}
}

// TestRole_WorkloadClass locks in that every known Role resolves to exactly
// one WorkloadClass, and that the mapping is exhaustive (no known role falls
// through to the unknown-role default by accident).
func TestRole_WorkloadClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role Role
		want WorkloadClass
	}{
		{RoleImplementation, ClassImplementation},
		{RolePlan, ClassImplementation},
		{RolePRFix, ClassCompletion},
		{RoleReview, ClassCompletion},
		{RoleFixReview, ClassCompletion},
		{RoleTestRunner, ClassCompletion},
		{RoleTestFix, ClassCompletion},
		{RoleTriage, ClassSystem},
		{RoleEval, ClassSystem},
		{RolePlanCritic, ClassSystem},
		{RoleHumanReview, ClassSystem},
		{RoleMonitor, ClassSystem},
		{RoleLoop, ClassSystem},
		{RoleOrchestrator, ClassSystem},
		// Unknown/empty role falls back to ClassImplementation, matching
		// RoleFromName's own fallback semantics.
		{Role(""), ClassImplementation},
		{Role("future"), ClassImplementation},
	}

	seen := make(map[Role]bool, len(tests))
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			t.Parallel()
			if got := tt.role.WorkloadClass(); got != tt.want {
				t.Fatalf("Role(%q).WorkloadClass() = %q, want %q", tt.role, got, tt.want)
			}
		})
		seen[tt.role] = true
	}

	allKnown := []Role{
		RoleTriage, RolePlan, RolePlanCritic, RoleEval, RoleLoop, RoleMonitor, RoleOrchestrator,
		RolePRFix, RoleReview, RoleFixReview, RoleTestRunner, RoleImplementation,
		RoleHumanReview, RoleTestFix,
	}
	for _, r := range allKnown {
		if !seen[r] {
			t.Fatalf("Role %q has no WorkloadClass test case", r)
		}
	}
}

// TestAllRolesReturnsACopy proves callers cannot mutate the canonical role set
// through the accessor — IsKnown reads the same backing list.
func TestAllRolesReturnsACopy(t *testing.T) {
	t.Parallel()
	got := AllRoles()
	got[0] = Role("clobbered")
	if !RoleTriage.IsKnown() {
		t.Fatal("mutating the AllRoles result corrupted the canonical role set")
	}
	if AllRoles()[0] != RoleTriage {
		t.Fatalf("AllRoles()[0] = %q, want %q", AllRoles()[0], RoleTriage)
	}
}

func BenchmarkRoleIsKnown(b *testing.B) {
	for b.Loop() {
		_ = RoleImplementation.IsKnown()
	}
}
