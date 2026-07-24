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

	desc, ok := LookupPathDescriptor("execution.agent.bash_timeout")
	if !ok {
		t.Fatal("LookupPathDescriptor(execution.agent.bash_timeout) = false")
	}
	if desc.RuntimePath != "agent.bash_timeout_seconds" {
		t.Fatalf("runtime path = %q, want agent.bash_timeout_seconds", desc.RuntimePath)
	}
	if len(desc.LegacyPaths) != 2 || desc.LegacyPaths[0] != "agent.bash_timeout_seconds" || desc.LegacyPaths[1] != "agent.bash_timeout" {
		t.Fatalf("legacy paths = %v, want [agent.bash_timeout_seconds agent.bash_timeout]", desc.LegacyPaths)
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

	fileCfg, err := ParseFileConfig([]byte("server:\n  auth_token: file-secret\n"))
	if err != nil {
		t.Fatalf("ParseFileConfig: %v", err)
	}
	explanation, err := ExplainPath("server.auth_token", fileCfg, Environment{}, cfg)
	if err != nil {
		t.Fatalf("ExplainPath: %v", err)
	}
	if explanation.Intent.Value != RedactedPlaceholder {
		t.Fatalf("intent value = %#v, want %q", explanation.Intent.Value, RedactedPlaceholder)
	}
	if explanation.Effective.Value != RedactedPlaceholder {
		t.Fatalf("effective value = %#v, want %q", explanation.Effective.Value, RedactedPlaceholder)
	}
	if !explanation.Intent.Redacted || !explanation.Effective.Redacted {
		t.Fatalf("secret explanation must redact both intent and effective values: %+v", explanation)
	}
}
