package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnattendedPostureGatesTheLoadPath pins the requirement where it has to
// hold to be non-droppable: inside the config load every entry point funnels
// through (server startup, -check-config preflight, and the fsnotify hot
// reload). A boot-time-only check is defeated by an operator editing
// config.yaml afterwards, which is the exact silent drift this guards.
func TestUnattendedPostureGatesTheLoadPath(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		require bool
		wantErr bool
	}{
		{
			name:    "unattended rejects an omitted sandbox_mode",
			yaml:    "schema_version: 2\nexecution:\n  agent:\n    provider: claude\n",
			require: true,
			wantErr: true,
		},
		{
			name:    "unattended rejects an empty sandbox_mode",
			yaml:    "schema_version: 2\nexecution:\n  agent:\n    sandbox_mode: \"\"\n",
			require: true,
			wantErr: true,
		},
		{
			name:    "unattended accepts an explicit enforce",
			yaml:    "schema_version: 2\nexecution:\n  agent:\n    sandbox_mode: enforce\n",
			require: true,
		},
		{
			name:    "unattended accepts an explicit report",
			yaml:    "schema_version: 2\nexecution:\n  agent:\n    sandbox_mode: report\n",
			require: true,
		},
		{
			name:    "attended keeps the report default on an omitted key",
			yaml:    "schema_version: 2\nexecution:\n  agent:\n    provider: claude\n",
			require: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SYBRA_HOME", home)
			if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
				panic("unreachable")
			}
			RequireExplicitSandboxMode(tc.require)
			t.Cleanup(func() { RequireExplicitSandboxMode(false) })

			_, err := LoadNoPersist()
			if tc.wantErr {
				if err == nil {
					t.Fatal("want load error, got nil")
					panic("unreachable")
				}
				if !strings.Contains(err.Error(), "agent.sandbox_mode is unset") {
					t.Fatalf("error = %v, want it to name agent.sandbox_mode", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
				panic("unreachable")
			}
		})
	}
}

// TestDefaultConfigSurvivesUnattendedRequirement guards the trap this check
// originally fell into: the built-in empty config legitimately has no
// sandbox_mode, so folding the requirement into ValidateResolvedConfig
// panicked every DefaultConfig caller in an unattended process.
func TestDefaultConfigSurvivesUnattendedRequirement(t *testing.T) {
	RequireExplicitSandboxMode(true)
	t.Cleanup(func() { RequireExplicitSandboxMode(false) })
	if cfg := DefaultConfig(); cfg == nil {
		t.Fatal("DefaultConfig returned nil under the unattended requirement")
		panic("unreachable")
	}
}
