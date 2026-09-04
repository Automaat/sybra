package sybra

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestGetSettingsRedactsAndPreservesGitHubWebhookSecrets(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.GitHub.Webhook.Secret = "github-secret"
	svc.cfg.GitHub.Webhook.TaskSecret = "task-secret"
	svc.persisted = cloneConfig(svc.cfg)
	writeConfigYAML(t, cfgPath, svc.cfg)

	settings := svc.GetSettings()
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "github-secret") || strings.Contains(string(data), "task-secret") {
		t.Fatalf("GetSettings leaked webhook secrets: %s", data)
	}
	if settings.GitHub.Webhook.Secret != config.RedactedPlaceholder {
		t.Fatalf("GitHub webhook secret = %q, want redaction placeholder", settings.GitHub.Webhook.Secret)
	}
	if settings.GitHub.Webhook.TaskSecret != config.RedactedPlaceholder {
		t.Fatalf("task webhook secret = %q, want redaction placeholder", settings.GitHub.Webhook.TaskSecret)
	}

	settings.GitHub.Enabled = !settings.GitHub.Enabled
	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if svc.persisted.GitHub.Webhook.Secret != "github-secret" {
		t.Fatalf("GitHub webhook secret = %q, want preserved value", svc.persisted.GitHub.Webhook.Secret)
	}
	if svc.persisted.GitHub.Webhook.TaskSecret != "task-secret" {
		t.Fatalf("task webhook secret = %q, want preserved value", svc.persisted.GitHub.Webhook.TaskSecret)
	}

	settings = svc.GetSettings()
	settings.GitHub.Webhook.Secret = ""
	settings.GitHub.Webhook.TaskSecret = ""
	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings clearing secrets: %v", err)
	}
	if svc.persisted.GitHub.Webhook.Secret != "" {
		t.Fatalf("GitHub webhook secret = %q, want cleared value", svc.persisted.GitHub.Webhook.Secret)
	}
	if svc.persisted.GitHub.Webhook.TaskSecret != "" {
		t.Fatalf("task webhook secret = %q, want cleared value", svc.persisted.GitHub.Webhook.TaskSecret)
	}
}

func TestUpdateSettings_ValidationRejectsBadFallbackModel(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	settings := svc.GetSettings()
	settings.Agent.FallbackModel = "bad model; rm -rf /"

	if _, err := svc.UpdateSettings(settings); err == nil {
		t.Error("expected validation error for invalid fallback model, got nil")
	}

	// Valid model string must be accepted.
	settings.Agent.FallbackModel = "claude-sonnet-4-6"
	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Errorf("UpdateSettings with valid fallback model: %v", err)
	}
}

func TestUpdateSettings_ValidationRejectsLogRetentionValuesBelowDisableSentinel(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	tests := []struct {
		name string
		mut  func(*AppSettings)
		want string
	}{
		{
			name: "retention days",
			mut:  func(s *AppSettings) { s.Agent.LogRetentionDays = -2 },
			want: "logRetentionDays must be -1 or greater",
		},
		{
			name: "gzip after days",
			mut:  func(s *AppSettings) { s.Agent.LogGzipAfterDays = -2 },
			want: "logGzipAfterDays must be -1 or greater",
		},
		{
			name: "max size mb",
			mut:  func(s *AppSettings) { s.Agent.LogRetentionMaxSizeMB = -2 },
			want: "logRetentionMaxSizeMb must be -1 or greater",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			settings := svc.GetSettings()
			tc.mut(&settings)
			_, err := svc.UpdateSettings(settings)
			if err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestUpdateSettings_AcceptsLogRetentionDisableSentinels(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	settings := svc.GetSettings()
	settings.Agent.LogRetentionDays = -1
	settings.Agent.LogGzipAfterDays = -1
	settings.Agent.LogRetentionMaxSizeMB = -1

	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if svc.cfg.Agent.LogRetentionDays != -1 ||
		svc.cfg.Agent.LogGzipAfterDays != -1 ||
		svc.cfg.Agent.LogRetentionMaxSizeMB != -1 {
		t.Fatalf("log retention sentinels not persisted: %+v", svc.cfg.Agent)
	}
}

func TestUpdateSettings_PreservesAttachmentLimitWhenOmitted(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.Attachments.MaxSizeMB = 12
	writeConfigYAML(t, cfgPath, svc.cfg)

	settings := svc.GetSettings()
	settings.Attachments = config.AttachmentConfig{}

	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if got := svc.cfg.Attachments.MaxSizeMB; got != 12 {
		t.Fatalf("Attachments.MaxSizeMB = %d, want preserved value 12", got)
	}
}
