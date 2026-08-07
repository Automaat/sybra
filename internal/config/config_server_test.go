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
	t.Setenv("SYBRA_WEBHOOK_SECRET", "")
	t.Setenv("SYBRA_GITHUB_WEBHOOK_SECRET", "")
}

func TestLoadGeneratesAndPersistsServerAuthToken(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if cfg.Server.AuthToken == "" {
		t.Fatal("Server.AuthToken should be auto-generated when unset")
	}
	if len(cfg.Server.AuthToken) != 64 {
		t.Errorf("AuthToken length = %d, want 64 (32 random bytes, hex-encoded)", len(cfg.Server.AuthToken))
	}

	// The generated token must be persisted to its own file, never to
	// config.yaml — see #2180.
	tokenFile, err := os.ReadFile(AuthTokenPath())
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if strings.TrimSpace(string(tokenFile)) != cfg.Server.AuthToken {
		t.Error("generated token was not persisted to AuthTokenPath()")
	}
	cfgData, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if strings.Contains(string(cfgData), cfg.Server.AuthToken) {
		t.Error("generated token leaked into config.yaml")
	}

	// A second Load() must reuse the persisted token rather than rotating it.
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
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
		panic("unreachable")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
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
		panic("unreachable")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
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
		panic("unreachable")
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
		panic("unreachable")
	}

	cfg, err := LoadNoPersist()
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if cfg.Server.AuthToken != "" {
		t.Fatalf("AuthToken = %q, want empty for read-only loads", cfg.Server.AuthToken)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if !strings.Contains(string(data), "allowed_origins") {
		t.Fatalf("config.yaml changed unexpectedly: %s", data)
	}
	if strings.Contains(string(data), "auth_token") {
		t.Fatalf("LoadNoPersist should not persist auth_token, got %s", data)
	}
	if _, err := os.Stat(AuthTokenPath()); !os.IsNotExist(err) {
		t.Fatalf("LoadNoPersist should not create AuthTokenPath(), stat err = %v", err)
	}
}

func TestLoadWebhookDefaults(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if cfg.GitHub.Webhook.Enabled {
		t.Fatal("GitHub.Webhook.Enabled = true, want false by default")
	}
	if cfg.GitHub.Webhook.Port != DefaultWebhookPort {
		t.Fatalf("GitHub.Webhook.Port = %d, want %d", cfg.GitHub.Webhook.Port, DefaultWebhookPort)
	}
	if cfg.GitHub.Webhook.Secret != "" || cfg.GitHub.Webhook.TaskSecret != "" {
		t.Fatalf("GitHub.Webhook secrets = (%q, %q), want empty by default",
			cfg.GitHub.Webhook.Secret, cfg.GitHub.Webhook.TaskSecret)
	}
	if cfg.GitHub.Webhook.TaskEnabled {
		t.Fatal("GitHub.Webhook.TaskEnabled = true, want false by default")
	}
	if cfg.GitHub.Webhook.CommandPrefix != DefaultGitHubWebhookCommandPrefix {
		t.Fatalf("GitHub.Webhook.CommandPrefix = %q, want %q", cfg.GitHub.Webhook.CommandPrefix, DefaultGitHubWebhookCommandPrefix)
	}
}

func TestLoadWebhookSecretEnvOverride(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)
	t.Setenv("SYBRA_WEBHOOK_SECRET", "env-webhook-secret")

	yaml := []byte("webhook:\n  enabled: true\n  port: 9092\n  secret: file-secret\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if cfg.GitHub.Webhook.TaskSecret != "env-webhook-secret" {
		t.Fatalf("GitHub.Webhook.TaskSecret = %q, want env override", cfg.GitHub.Webhook.TaskSecret)
	}
	if !cfg.GitHub.Webhook.Enabled || cfg.GitHub.Webhook.Port != 9092 {
		t.Fatalf("legacy webhook listener = (%v, %d), want (true, 9092)",
			cfg.GitHub.Webhook.Enabled, cfg.GitHub.Webhook.Port)
	}
	if !cfg.GitHub.Webhook.TaskEnabled {
		t.Fatal("legacy webhook did not preserve the generic task route")
	}
}

func TestLoadGitHubWebhookEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	isolateServerEnv(t)
	t.Setenv("SYBRA_HOME", dir)
	t.Setenv("SYBRA_GITHUB_WEBHOOK_SECRET", "env-github-secret")

	yaml := []byte("github:\n  webhook:\n    enabled: true\n    port: 9091\n    secret: file-secret\n    command_prefix: /file-agent\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if cfg.GitHub.Webhook.Secret != "env-github-secret" {
		t.Fatalf("GitHub.Webhook.Secret = %q, want env override", cfg.GitHub.Webhook.Secret)
	}
	if cfg.GitHub.Webhook.CommandPrefix != "/file-agent" {
		t.Fatalf("GitHub.Webhook.CommandPrefix = %q, want /file-agent", cfg.GitHub.Webhook.CommandPrefix)
	}
	if !cfg.GitHub.Webhook.Enabled || cfg.GitHub.Webhook.Port != 9091 {
		t.Fatalf("GitHub.Webhook listener = (%v, %d), want (true, 9091)",
			cfg.GitHub.Webhook.Enabled, cfg.GitHub.Webhook.Port)
	}
}
