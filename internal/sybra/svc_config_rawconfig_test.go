package sybra

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestGetSettings_ExposesExpandedSections(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.GitHub.Enabled = true
	svc.cfg.Monitor.Enabled = true
	svc.cfg.Triage.PollSeconds = 45
	svc.cfg.ProjectTypes = []string{"pet"}
	writeConfigYAML(t, cfgPath, svc.cfg)

	got := svc.GetSettings()
	if !got.GitHub.Enabled || !got.Monitor.Enabled {
		t.Error("expanded sections not surfaced in GetSettings")
	}
	if got.Triage.PollSeconds != 45 {
		t.Errorf("triage poll = %d, want 45", got.Triage.PollSeconds)
	}
	if len(got.ProjectTypes) != 1 || got.ProjectTypes[0] != "pet" {
		t.Errorf("projectTypes = %v, want [pet]", got.ProjectTypes)
	}
}

func TestGetDefaultSettings_MatchesDefaultConfig(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	def := svc.GetDefaultSettings()
	want := config.DefaultConfig()
	if def.GitHub.Enabled != want.GitHub.Enabled {
		t.Errorf("default github.enabled = %v, want %v", def.GitHub.Enabled, want.GitHub.Enabled)
	}
	if def.Agent.MaxConcurrent != want.Agent.MaxConcurrent {
		t.Errorf("default maxConcurrent = %d, want %d", def.Agent.MaxConcurrent, want.Agent.MaxConcurrent)
	}
	if def.Audit.RetentionDays != want.Audit.RetentionDays {
		t.Errorf("default audit retention = %d, want %d", def.Audit.RetentionDays, want.Audit.RetentionDays)
	}
}

func TestUpdateSettings_RoundTripsExpandedSections(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	s := svc.GetSettings()
	s.GitHub.Enabled = true
	s.GitHub.PollerRole = "secondary"
	s.Monitor.Enabled = true
	s.Monitor.IntervalSeconds = 600
	s.Umbrella.Enabled = true
	s.Testing.MaxConcurrent = 4
	s.ProjectTypes = []string{"work"}

	if _, err := svc.UpdateSettings(s); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if svc.cfg.GitHub.PollerRole == "secondary" ||
		svc.cfg.Monitor.IntervalSeconds == 600 || svc.cfg.Umbrella.Enabled {
		t.Errorf("restart-required sections unexpectedly became active: github=%+v monitor=%+v umbrella=%+v", svc.cfg.GitHub, svc.cfg.Monitor, svc.cfg.Umbrella)
	}
	if svc.cfg.Testing.MaxConcurrent != 4 {
		t.Errorf("hot testing settings not applied in-memory: %+v", svc.cfg.Testing)
	}
	got := svc.GetSettings()
	if got.GitHub.PollerRole != "secondary" || !got.Monitor.Enabled ||
		got.Monitor.IntervalSeconds != 600 || !got.Umbrella.Enabled {
		t.Errorf("persisted expanded sections not retained: github=%+v monitor=%+v umbrella=%+v", got.GitHub, got.Monitor, got.Umbrella)
	}
	if len(got.ProjectTypes) != 1 || got.ProjectTypes[0] != "work" {
		t.Errorf("persisted projectTypes = %v, want [work]", got.ProjectTypes)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "poller_role: secondary") {
		t.Error("expanded section not persisted to disk")
	}
}

func TestGetRawConfig_ReturnsFileContents(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	raw, err := svc.GetRawConfig()
	if err != nil {
		t.Fatalf("GetRawConfig: %v", err)
	}
	if !strings.Contains(raw, "agent:") || !strings.Contains(raw, "provider: claude") {
		t.Errorf("raw config missing expected keys:\n%s", raw)
	}
}

func TestSaveRawConfig_ValidRoundTrip(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	raw, err := svc.GetRawConfig()
	if err != nil {
		t.Fatal(err)
	}
	// A user comment must survive the save verbatim (raw bytes, not re-marshalled).
	edited := "# my hand-written note\n" + strings.Replace(raw, "max_files: 5", "max_files: 9", 1)

	if err := svc.SaveRawConfig(edited); err != nil {
		t.Fatalf("SaveRawConfig: %v", err)
	}
	if svc.cfg.Logging.MaxFiles != 9 {
		t.Errorf("in-memory maxFiles = %d, want 9 (hot reload)", svc.cfg.Logging.MaxFiles)
	}
	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "# my hand-written note") {
		t.Error("raw save did not preserve the user comment")
	}
	if !strings.Contains(string(saved), "max_files: 9") {
		t.Error("raw save did not persist the edit")
	}
}

func TestSaveRawConfig_PreservesServerAuthTokenWhenOmitted(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.Server.AuthToken = "persist-me"
	svc.persisted = cloneConfig(svc.cfg)
	writeConfigYAML(t, cfgPath, svc.cfg)

	raw := "schema_version: 2\nagent:\n  bash_timeout: 2m\n"
	if err := svc.SaveRawConfig(raw); err != nil {
		t.Fatalf("SaveRawConfig: %v", err)
	}
	if svc.cfg.Server.AuthToken != "persist-me" {
		t.Fatalf("in-memory auth token = %q, want persist-me", svc.cfg.Server.AuthToken)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	savedText := string(saved)
	if !strings.Contains(savedText, "auth_token: persist-me") {
		t.Fatalf("saved config missing preserved auth token:\n%s", savedText)
	}
}

func TestSaveRawConfig_PreservesFormattingWhenBuiltinVersionMissing(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	raw, err := svc.GetRawConfig()
	if err != nil {
		t.Fatal(err)
	}
	edited := "# keep this comment\n" + regexp.MustCompile(`(?m)^\s*builtin_version: \d+\n`).ReplaceAllString(raw, "")
	edited = strings.Replace(edited, "max_files: 5", "max_files: 7", 1)

	if err := svc.SaveRawConfig(edited); err != nil {
		t.Fatalf("SaveRawConfig: %v", err)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	savedText := string(saved)
	if !strings.Contains(savedText, "# keep this comment") {
		t.Fatal("raw save lost the preserved comment")
	}
	if strings.Contains(savedText, "builtin_version:") {
		t.Fatal("hot reload rewrote the raw config with builtin_version")
	}
	if !strings.Contains(savedText, "max_files: 7") {
		t.Fatal("raw save did not persist the edit")
	}
}

func TestSaveRawConfig_RejectsInvalidYAML(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)
	before, _ := os.ReadFile(cfgPath)

	if err := svc.SaveRawConfig("agent:\n  : : not valid yaml"); err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
	after, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(before, after) {
		t.Error("invalid raw save must not touch disk")
	}
}

func TestSaveRawConfig_RejectsOutOfRangeValue(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	raw, err := svc.GetRawConfig()
	if err != nil {
		t.Fatal(err)
	}
	// max_concurrent must be 1–100; 999 fails validateSettings.
	bad := strings.Replace(raw, "max_concurrent: 3", "max_concurrent: 999", 1)
	if err := svc.SaveRawConfig(bad); err == nil {
		t.Error("expected validation error for out-of-range max_concurrent, got nil")
	}
	if svc.cfg.Agent.MaxConcurrent == 999 {
		t.Error("rejected raw save must not mutate in-memory config")
	}
}
