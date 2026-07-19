package config

import (
	"slices"
	"testing"
)

func TestSecretYAMLPaths(t *testing.T) {
	got := SecretYAMLPaths()
	for _, want := range []string{"server.auth_token", "webhook.secret"} {
		if !slices.Contains(got, want) {
			t.Fatalf("SecretYAMLPaths() missing %q: %v", want, got)
		}
		if !IsSecretYAMLPath(want) {
			t.Fatalf("IsSecretYAMLPath(%q) = false", want)
		}
	}
	if IsSecretYAMLPath("server.allowed_origins") {
		t.Fatal("non-secret server.allowed_origins reported as secret")
	}
}

func TestRedactedCopy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.AuthToken = "server-secret"
	cfg.Webhook.Secret = "webhook-secret"

	redacted := RedactedCopy(cfg)
	if redacted.Server.AuthToken != RedactedPlaceholder {
		t.Fatalf("server auth token = %q, want %q", redacted.Server.AuthToken, RedactedPlaceholder)
	}
	if redacted.Webhook.Secret != RedactedPlaceholder {
		t.Fatalf("webhook secret = %q, want %q", redacted.Webhook.Secret, RedactedPlaceholder)
	}
	if redacted.Logging.Level != cfg.Logging.Level {
		t.Fatalf("non-secret field changed: logging.level %q -> %q", cfg.Logging.Level, redacted.Logging.Level)
	}
}
