package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func mustUnmarshalOrchestrator(t *testing.T, in string) OrchestratorConfig {
	t.Helper()
	cfg := DefaultConfig().Orchestrator
	if err := yaml.Unmarshal([]byte(in), &cfg); err != nil {
		t.Fatalf("unmarshal orchestrator config: %v", err)
	}
	return cfg
}

func TestNormalizeInstanceRole(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to full", in: "", want: InstanceRoleFull},
		{name: "full", in: "full", want: InstanceRoleFull},
		{name: "agent-only", in: "agent-only", want: InstanceRoleAgentOnly},
		{name: "uppercase full", in: "Full", want: InstanceRoleFull},
		{name: "padded agent-only", in: "  agent-only ", want: InstanceRoleAgentOnly},
		{name: "unknown rejected", in: "worker", wantErr: true},
		{name: "near-miss rejected", in: "agentonly", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeInstanceRole(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeInstanceRole(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeInstanceRole(%q): unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeInstanceRole(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestOrchestratorConfig_RoleGates(t *testing.T) {
	ptr := func(b bool) *bool { return &b }

	tests := []struct {
		name          string
		cfg           OrchestratorConfig
		wantRole      string
		wantOrch      bool
		wantScheduler bool
	}{
		{
			name:          "zero value runs the scheduler, never the brain",
			cfg:           OrchestratorConfig{},
			wantRole:      InstanceRoleFull,
			wantOrch:      false,
			wantScheduler: true,
		},
		{
			name:          "default config is full, brain stays off",
			cfg:           DefaultConfig().Orchestrator,
			wantRole:      InstanceRoleFull,
			wantOrch:      false,
			wantScheduler: true,
		},
		{
			name:          "agent-only fails closed on both",
			cfg:           OrchestratorConfig{Role: InstanceRoleAgentOnly},
			wantRole:      InstanceRoleAgentOnly,
			wantOrch:      false,
			wantScheduler: false,
		},
		{
			name:          "invalid role falls back to full scheduler, brain stays off",
			cfg:           OrchestratorConfig{Role: "nonsense"},
			wantRole:      InstanceRoleFull,
			wantOrch:      false,
			wantScheduler: true,
		},
		{
			name:          "explicit enabled overrides agent-only",
			cfg:           OrchestratorConfig{Role: InstanceRoleAgentOnly, Enabled: ptr(true)},
			wantRole:      InstanceRoleAgentOnly,
			wantOrch:      true,
			wantScheduler: false,
		},
		{
			name:          "explicit scheduler_enabled overrides agent-only",
			cfg:           OrchestratorConfig{Role: InstanceRoleAgentOnly, SchedulerEnabled: ptr(true)},
			wantRole:      InstanceRoleAgentOnly,
			wantOrch:      false,
			wantScheduler: true,
		},
		{
			name:          "explicit false parks a full instance",
			cfg:           OrchestratorConfig{Role: InstanceRoleFull, Enabled: ptr(false), SchedulerEnabled: ptr(false)},
			wantRole:      InstanceRoleFull,
			wantOrch:      false,
			wantScheduler: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.InstanceRole(); got != tt.wantRole {
				t.Errorf("InstanceRole() = %q, want %q", got, tt.wantRole)
			}
			if got := tt.cfg.RunsOrchestrator(); got != tt.wantOrch {
				t.Errorf("RunsOrchestrator() = %v, want %v", got, tt.wantOrch)
			}
			if got := tt.cfg.RunsScheduler(); got != tt.wantScheduler {
				t.Errorf("RunsScheduler() = %v, want %v", got, tt.wantScheduler)
			}
		})
	}
}

func TestOrchestratorConfig_RoleFromYAML(t *testing.T) {
	tests := []struct {
		name          string
		yaml          string
		wantOrch      bool
		wantScheduler bool
	}{
		{
			name:          "agent-only role only",
			yaml:          "role: agent-only\n",
			wantOrch:      false,
			wantScheduler: false,
		},
		{
			name:          "agent-only with brain re-enabled",
			yaml:          "role: agent-only\nenabled: true\n",
			wantOrch:      true,
			wantScheduler: false,
		},
		{
			name:          "absent block keeps scheduler full, brain off",
			yaml:          "dispatch_interval_seconds: 10\n",
			wantOrch:      false,
			wantScheduler: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustUnmarshalOrchestrator(t, tt.yaml)
			if got := cfg.RunsOrchestrator(); got != tt.wantOrch {
				t.Errorf("RunsOrchestrator() = %v, want %v", got, tt.wantOrch)
			}
			if got := cfg.RunsScheduler(); got != tt.wantScheduler {
				t.Errorf("RunsScheduler() = %v, want %v", got, tt.wantScheduler)
			}
		})
	}
}

// TestOrchestratorConfig_BrainUpgradeBehavior locks in the brain-auto-start
// default across every config generation this instance might load: a legacy
// pre-Role file, a v2 file that only sets the namespaced Role, and both
// explicit overrides. In every case except an explicit `enabled: true`, the
// brain must stay off — an omitted or role-only config must never resurrect
// automatic orchestrator startup.
func TestOrchestratorConfig_BrainUpgradeBehavior(t *testing.T) {
	tests := []struct {
		name          string
		yaml          string
		wantOrch      bool
		wantScheduler bool
	}{
		{
			name:          "legacy: no orchestrator block at all",
			yaml:          "",
			wantOrch:      false,
			wantScheduler: true,
		},
		{
			name:          "v2: role full, no enabled key",
			yaml:          "role: full\n",
			wantOrch:      false,
			wantScheduler: true,
		},
		{
			name:          "v2: role agent-only, no enabled key",
			yaml:          "role: agent-only\n",
			wantOrch:      false,
			wantScheduler: false,
		},
		{
			name:          "explicit-true: enabled true on role full",
			yaml:          "role: full\nenabled: true\n",
			wantOrch:      true,
			wantScheduler: true,
		},
		{
			name:          "explicit-true: enabled true on role agent-only",
			yaml:          "role: agent-only\nenabled: true\n",
			wantOrch:      true,
			wantScheduler: false,
		},
		{
			name:          "explicit-false: enabled false on role full",
			yaml:          "role: full\nenabled: false\n",
			wantOrch:      false,
			wantScheduler: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustUnmarshalOrchestrator(t, tt.yaml)
			if got := cfg.RunsOrchestrator(); got != tt.wantOrch {
				t.Errorf("RunsOrchestrator() = %v, want %v", got, tt.wantOrch)
			}
			if got := cfg.RunsScheduler(); got != tt.wantScheduler {
				t.Errorf("RunsScheduler() = %v, want %v", got, tt.wantScheduler)
			}
		})
	}
}
