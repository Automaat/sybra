package agent

import (
	"testing"
)

// TestPrepareRunConfig_ResolvesReasoningEffort pins the resolution order every
// dispatch site now inherits from the Manager: an explicitly pinned effort (an
// experiment assignment or the task's own reasoning_effort, both resolved
// upstream) > the operator's agent.role_effort override > the role's built-in
// baseline > the global default.
//
// The no-explicit-effort rows are the regression guard for #2784: seven of the
// nine RunConfig construction sites set no effort at all, and the Manager used
// to stamp the global "medium" on them regardless of role.
func TestPrepareRunConfig_ResolvesReasoningEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		role       Role
		agentName  string
		explicit   string
		roleEffort map[string]string
		want       string
	}{
		{name: "review inherits its high baseline", role: RoleReview, want: "high"},
		{name: "plan inherits its high baseline", role: RolePlan, want: "high"},
		{name: "monitor inherits its low baseline", role: RoleMonitor, want: "low"},
		{name: "human-review inherits its low baseline", role: RoleHumanReview, want: "low"},
		{name: "implementation falls back to the global default", role: RoleImplementation, want: DefaultReasoningEffort},
		{name: "test-runner falls back to the global default", role: RoleTestRunner, want: DefaultReasoningEffort},
		{
			name:     "explicit effort beats the role baseline",
			role:     RoleReview,
			explicit: "low",
			want:     "low",
		},
		{
			name:       "explicit effort beats the config override",
			role:       RoleReview,
			explicit:   "low",
			roleEffort: map[string]string{"review": "xhigh"},
			want:       "low",
		},
		{
			name:       "config override beats the role baseline",
			role:       RoleReview,
			roleEffort: map[string]string{"review": "medium"},
			want:       "medium",
		},
		{
			name:       "config override applies to a role with no baseline",
			role:       RoleImplementation,
			roleEffort: map[string]string{"implementation": "high"},
			want:       "high",
		},
		{
			name:       "invalid override falls back to the role baseline",
			role:       RoleReview,
			roleEffort: map[string]string{"review": "extreme"},
			want:       "high",
		},
		{
			name:       "empty override falls back to the role baseline",
			role:       RoleReview,
			roleEffort: map[string]string{"review": ""},
			want:       "high",
		},
		{
			name:       "override for another role does not leak",
			role:       RoleReview,
			roleEffort: map[string]string{"implementation": "low"},
			want:       "high",
		},
		{
			// Effort must resolve after ResolveRunRole, so a legacy
			// prefix-only config still gets its role's baseline.
			name:      "prefix-only name resolves through its role",
			agentName: "review:Some task title",
			want:      "high",
		},
		{
			name:      "unprefixed name is implementation and takes the global default",
			agentName: "Some task title",
			want:      DefaultReasoningEffort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newParseTestManager(t, ManagerConfig{Runtime: ManagerRuntimeConfig{RoleEffort: tt.roleEffort}})
			cfg, _, err := m.prepareRunConfig(RunConfig{
				Role:            tt.role,
				Name:            tt.agentName,
				Provider:        "claude",
				Mode:            "headless",
				Prompt:          "hi",
				Dir:             t.TempDir(),
				ReasoningEffort: tt.explicit,
			})
			if err != nil {
				t.Fatalf("prepareRunConfig: %v", err)
				panic("unreachable")
			}
			if cfg.ReasoningEffort != tt.want {
				t.Errorf("ReasoningEffort = %q, want %q", cfg.ReasoningEffort, tt.want)
			}
		})
	}
}

// TestPrepareRunConfig_RoleEffortReachesProviderArgv proves the resolved level
// is not merely stamped on the RunConfig but actually reaches the provider CLI
// — a verifier role dispatched with no explicit effort must launch at its
// baseline, not the global default.
func TestPrepareRunConfig_RoleEffortReachesProviderArgv(t *testing.T) {
	t.Parallel()
	m := newParseTestManager(t)
	cfg, prov, err := m.prepareRunConfig(RunConfig{
		Role:     RoleReview,
		Provider: "claude",
		Mode:     "headless",
		Prompt:   "hi",
		Dir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
		panic("unreachable")
	}
	a := newRunningAgent("a", cfg, prov, func() {})
	_, args, _, _, err := buildHeadlessInvocation(a, cfg)
	if err != nil {
		t.Fatalf("buildHeadlessInvocation: %v", err)
		panic("unreachable")
	}
	if !hasArgPair(args, "--effort", "high") {
		t.Fatalf("expected --effort high in args; got %v", args)
	}
}

// TestReplaceRuntimeConfig_HotUpdatesRoleEffort covers agent.role_effort's
// documented "hot" reload class: an operator edit must land on the next run
// without a restart, and clearing the map must restore the built-in baseline.
func TestReplaceRuntimeConfig_HotUpdatesRoleEffort(t *testing.T) {
	t.Parallel()
	m := newParseTestManager(t, ManagerConfig{Runtime: ManagerRuntimeConfig{
		RoleEffort: map[string]string{"review": "low"},
	}})
	if got := m.resolveReasoningEffort(RoleReview, ""); got != "low" {
		t.Fatalf("resolveReasoningEffort before reload = %q, want %q", got, "low")
	}

	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{
		RoleEffort: map[string]string{"review": "xhigh"},
	}); err != nil {
		t.Fatalf("ReplaceRuntimeConfig: %v", err)
	}
	if got := m.resolveReasoningEffort(RoleReview, ""); got != "xhigh" {
		t.Fatalf("resolveReasoningEffort after reload = %q, want %q", got, "xhigh")
	}

	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{}); err != nil {
		t.Fatalf("ReplaceRuntimeConfig clear: %v", err)
	}
	if got := m.resolveReasoningEffort(RoleReview, ""); got != "high" {
		t.Fatalf("resolveReasoningEffort after clear = %q, want built-in %q", got, "high")
	}
}

// TestNewManager_CopiesRoleEffort guards against the Manager aliasing the
// caller's map: a later mutation by the config owner must not silently retune
// live dispatch.
func TestNewManager_CopiesRoleEffort(t *testing.T) {
	t.Parallel()
	src := map[string]string{"review": "low"}
	m := newParseTestManager(t, ManagerConfig{Runtime: ManagerRuntimeConfig{RoleEffort: src}})
	src["review"] = "xhigh"
	if got := m.resolveReasoningEffort(RoleReview, ""); got != "low" {
		t.Fatalf("resolveReasoningEffort = %q, want %q (map must be cloned)", got, "low")
	}
}

func hasArgPair(args []string, key, value string) bool {
	for i := range len(args) - 1 {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
