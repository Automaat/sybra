package sybra

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestGetSettings_ExposesExpandedSectionsAndTokenFlag(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.Todoist.APIToken = "tok"
	svc.cfg.GitHub.Enabled = true
	svc.cfg.Monitor.Enabled = true
	svc.cfg.Triage.PollSeconds = 45
	svc.cfg.ProjectTypes = []string{"pet"}
	writeConfigYAML(t, cfgPath, svc.cfg)

	got := svc.GetSettings()
	if !got.TodoistTokenSet {
		t.Error("TodoistTokenSet should be true when a token is stored")
	}
	if got.Todoist.APIToken != "" {
		t.Error("token must still be redacted")
	}
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

	if err := svc.UpdateSettings(s); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if svc.cfg.GitHub.PollerRole != "secondary" || !svc.cfg.Monitor.Enabled ||
		svc.cfg.Monitor.IntervalSeconds != 600 || !svc.cfg.Umbrella.Enabled ||
		svc.cfg.Testing.MaxConcurrent != 4 {
		t.Errorf("expanded sections not applied in-memory: %+v", svc.cfg.GitHub)
	}
	if len(svc.cfg.ProjectTypes) != 1 || svc.cfg.ProjectTypes[0] != "work" {
		t.Errorf("projectTypes = %v, want [work]", svc.cfg.ProjectTypes)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "poller_role: secondary") {
		t.Error("expanded section not persisted to disk")
	}
}

func TestUpdateSettings_TodoistPollInterval(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.Todoist.APIToken = "tok"
	writeConfigYAML(t, cfgPath, svc.cfg)

	s := svc.GetSettings()
	s.Todoist.Enabled = true

	// Out-of-range is rejected (no silent by-value coercion).
	s.Todoist.PollSeconds = 5
	if err := svc.UpdateSettings(s); err == nil {
		t.Error("expected error for poll interval below 30, got nil")
	}
	s.Todoist.PollSeconds = 99999
	if err := svc.UpdateSettings(s); err == nil {
		t.Error("expected error for poll interval above 3600, got nil")
	}

	// 0 means "use default" and is accepted; in-range values pass.
	s.Todoist.PollSeconds = 0
	if err := svc.UpdateSettings(s); err != nil {
		t.Errorf("poll interval 0 should be accepted: %v", err)
	}
	s.Todoist.PollSeconds = 300
	if err := svc.UpdateSettings(s); err != nil {
		t.Errorf("poll interval 300 should be accepted: %v", err)
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

func TestSaveRawConfig_PreservesFormattingWhenBuiltinVersionMissing(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	raw, err := svc.GetRawConfig()
	if err != nil {
		t.Fatal(err)
	}
	edited := "# keep this comment\n" + strings.Replace(raw, "builtin_version: 2\n", "", 1)
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
