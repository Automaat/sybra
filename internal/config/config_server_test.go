package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateServerEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SYBRA_AUTH_TOKEN", "")
	t.Setenv("SYBRA_ALLOWED_ORIGINS", "")
}

func TestLoadGeneratesAndPersistsServerAuthToken(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AuthToken == "" {
		t.Fatal("Server.AuthToken should be auto-generated when unset")
	}
	if len(cfg.Server.AuthToken) != 64 {
		t.Errorf("AuthToken length = %d, want 64 (32 random bytes, hex-encoded)", len(cfg.Server.AuthToken))
	}

	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), cfg.Server.AuthToken) {
		t.Error("generated token was not persisted to config.yaml")
	}

	// A second Load() must reuse the persisted token rather than rotating it.
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Server.AuthToken != cfg.Server.AuthToken {
		t.Error("AuthToken rotated across restarts — should be persisted and stable")
	}
}

func TestLoadPreservesExplicitServerAuthToken(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("server:\n  auth_token: my-explicit-token\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AuthToken != "my-explicit-token" {
		t.Errorf("AuthToken = %q, want unchanged explicit value", cfg.Server.AuthToken)
	}
}

func TestLoadServerAuthTokenEnvOverride(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)
	t.Setenv("SYBRA_ALLOWED_ORIGINS", "")
	t.Setenv("SYBRA_AUTH_TOKEN", "env-token")

	yaml := []byte("server:\n  auth_token: file-token\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AuthToken != "env-token" {
		t.Errorf("AuthToken = %q, want env override to win", cfg.Server.AuthToken)
	}
}

func TestLoadServerAllowedOriginsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)
	t.Setenv("SYBRA_AUTH_TOKEN", "")
	t.Setenv("SYBRA_ALLOWED_ORIGINS", "https://a.example, https://b.example")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a.example", "https://b.example"}
	if len(cfg.Server.AllowedOrigins) != len(want) {
		t.Fatalf("AllowedOrigins = %v, want %v", cfg.Server.AllowedOrigins, want)
	}
	for i, o := range want {
		if cfg.Server.AllowedOrigins[i] != o {
			t.Errorf("AllowedOrigins[%d] = %q, want %q", i, cfg.Server.AllowedOrigins[i], o)
		}
	}
}

func TestLoadNoPersistDoesNotGenerateServerAuthToken(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("server:\n  allowed_origins: [https://a.example]\n")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadNoPersist()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AuthToken != "" {
		t.Fatalf("AuthToken = %q, want empty for read-only loads", cfg.Server.AuthToken)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "allowed_origins") {
		t.Fatalf("config.yaml changed unexpectedly: %s", data)
	}
	if strings.Contains(string(data), "auth_token") {
		t.Fatalf("LoadNoPersist should not persist auth_token, got %s", data)
	}
}
