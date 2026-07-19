package sybra

import (
	"strings"
	"testing"
)

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
