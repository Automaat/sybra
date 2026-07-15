package sybra

import (
	"os"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestGetSettings_RedactsAPIToken(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	svc.cfg.Todoist = config.TodoistConfig{
		Enabled:     true,
		APIToken:    "secret-token-abc123",
		PollSeconds: 120,
	}
	writeConfigYAML(t, cfgPath, svc.cfg)

	got := svc.GetSettings()
	if got.Todoist.APIToken != "" {
		t.Errorf("GetSettings returned APIToken %q, want empty string", got.Todoist.APIToken)
	}
	if !got.Todoist.Enabled {
		t.Error("GetSettings should preserve Todoist.Enabled")
	}
}

func TestUpdateSettings_PreservesTokenWhenBlank(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	svc.cfg.Todoist = config.TodoistConfig{
		Enabled:     true,
		APIToken:    "existing-token",
		PollSeconds: 120,
	}
	writeConfigYAML(t, cfgPath, svc.cfg)

	// Frontend sends back what GetSettings returns — token is blank.
	settings := svc.GetSettings()
	if settings.Todoist.APIToken != "" {
		t.Fatalf("precondition: GetSettings returned non-empty token")
	}
	if err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	if svc.cfg.Todoist.APIToken != "existing-token" {
		t.Errorf("in-memory token = %q, want %q", svc.cfg.Todoist.APIToken, "existing-token")
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "existing-token") {
		t.Error("saved config does not contain the preserved token")
	}
}

func TestUpdateTodoistToken(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	if err := svc.UpdateTodoistToken("new-token-xyz"); err != nil {
		t.Fatalf("UpdateTodoistToken: %v", err)
	}

	if svc.cfg.Todoist.APIToken != "new-token-xyz" {
		t.Errorf("in-memory token = %q, want %q", svc.cfg.Todoist.APIToken, "new-token-xyz")
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "new-token-xyz") {
		t.Error("saved config does not contain the new token")
	}

	// GetSettings must still redact after UpdateTodoistToken.
	if got := svc.GetSettings().Todoist.APIToken; got != "" {
		t.Errorf("GetSettings returned token %q after UpdateTodoistToken, want empty", got)
	}
}

func TestUpdateTodoistToken_ClearsToken(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.Todoist.APIToken = "existing-token"
	writeConfigYAML(t, cfgPath, svc.cfg)

	if err := svc.UpdateTodoistToken(""); err != nil {
		t.Fatalf("UpdateTodoistToken empty: %v", err)
	}
	if svc.cfg.Todoist.APIToken != "" {
		t.Errorf("in-memory token = %q, want empty", svc.cfg.Todoist.APIToken)
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "existing-token") {
		t.Error("saved config still contains old token after clear")
	}
}

func TestUpdateSettings_ValidationRejectsBadFallbackModel(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	settings := svc.GetSettings()
	settings.Agent.FallbackModel = "bad model; rm -rf /"

	if err := svc.UpdateSettings(settings); err == nil {
		t.Error("expected validation error for invalid fallback model, got nil")
	}

	// Valid model string must be accepted.
	settings.Agent.FallbackModel = "claude-sonnet-4-6"
	if err := svc.UpdateSettings(settings); err != nil {
		t.Errorf("UpdateSettings with valid fallback model: %v", err)
	}
}

func TestUpdateSettings_ValidationRejectsEnabledWithNoToken(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.Todoist.APIToken = ""
	writeConfigYAML(t, cfgPath, svc.cfg)

	settings := svc.GetSettings()
	settings.Todoist.Enabled = true
	settings.Todoist.APIToken = ""

	if err := svc.UpdateSettings(settings); err == nil {
		t.Error("expected validation error for enabled todoist with no token, got nil")
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
			err := svc.UpdateSettings(settings)
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

	if err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if svc.cfg.Agent.LogRetentionDays != -1 ||
		svc.cfg.Agent.LogGzipAfterDays != -1 ||
		svc.cfg.Agent.LogRetentionMaxSizeMB != -1 {
		t.Fatalf("log retention sentinels not persisted: %+v", svc.cfg.Agent)
	}
}
