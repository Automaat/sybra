package agentorch

import (
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
)

func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vals []string
		want string
	}{
		{"all empty", []string{"", "", ""}, ""},
		{"first wins", []string{"a", "b", "c"}, "a"},
		{"skips leading empties", []string{"", "", "c"}, "c"},
		{"no args", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FirstNonEmpty(tt.vals...); got != tt.want {
				t.Errorf("FirstNonEmpty(%v) = %q, want %q", tt.vals, got, tt.want)
			}
		})
	}
}

func TestResolveRoleEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		role agent.Role
		cfg  *config.Config
		want string
	}{
		{"nil cfg falls back to role default", agent.RoleTriage, nil, "low"},
		{"no override falls back to role default", agent.RoleImplementation, &config.Config{}, "high"},
		{"role with no built-in default returns empty", agent.RolePlan, &config.Config{}, ""},
		{
			"config override wins",
			agent.RoleImplementation,
			&config.Config{Agent: config.AgentDefaults{RoleEffort: map[string]string{"implementation": "medium"}}},
			"medium",
		},
		{
			"invalid override ignored, falls back to role default",
			agent.RoleImplementation,
			&config.Config{Agent: config.AgentDefaults{RoleEffort: map[string]string{"implementation": "extreme"}}},
			"high",
		},
		{
			"override for unrelated role does not affect this role",
			agent.RoleTriage,
			&config.Config{Agent: config.AgentDefaults{RoleEffort: map[string]string{"implementation": "medium"}}},
			"low",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolveRoleEffort(tt.role, tt.cfg); got != tt.want {
				t.Errorf("ResolveRoleEffort(%q) = %q, want %q", tt.role, got, tt.want)
			}
		})
	}
}
