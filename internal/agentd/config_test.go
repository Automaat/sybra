package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/providerid"
)

func TestLoadConfigIsStrictAndKeepsCredentialsOutOfFile(t *testing.T) {
	t.Setenv("AGENTD_TOKEN", "leader-secret")
	path := filepath.Join(t.TempDir(), "agentd.yaml")
	data := `leader_url: https://leader.example.test
token_env: AGENTD_TOKEN
capacity: 2
providers: [CLAUDE, codex]
sandbox_mode: enforce
workspace_root: /var/lib/agentd/workspaces
state_root: /var/lib/agentd/state
spool_max_bytes: 1048576
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Providers, ","); got != "claude,codex" {
		t.Fatalf("providers = %q", got)
	}
	if strings.Contains(data, os.Getenv("AGENTD_TOKEN")) {
		t.Fatal("leader credential was persisted in configuration")
	}

	if err := os.WriteFile(path, []byte(data+"unknown_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "field unknown_field not found") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestConfigRejectsUnsafeBoundaries(t *testing.T) {
	t.Setenv("AGENTD_TOKEN", "leader-secret")
	base := Config{
		LeaderURL: "https://leader.example.test", TokenEnv: "AGENTD_TOKEN", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "enforce", WorkspaceRoot: "/var/lib/agentd/workspaces",
		StateRoot: "/var/lib/agentd/state", SpoolMaxBytes: 1 << 20,
	}

	nonTLS := base
	nonTLS.LeaderURL = "http://leader.example.test"
	if err := nonTLS.Validate(); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("non-TLS error = %v", err)
	}

	unknownProvider := base
	unknownProvider.Providers = []string{"mystery"}
	if err := unknownProvider.Validate(); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("unknown provider error = %v", err)
	}

	leakedToken := base
	leakedToken.SecretEnv = map[string]string{"run/example/input": "AGENTD_TOKEN"}
	if err := leakedToken.Validate(); err == nil || !strings.Contains(err.Error(), "leader token environment") {
		t.Fatalf("leader-token reuse error = %v", err)
	}
}
