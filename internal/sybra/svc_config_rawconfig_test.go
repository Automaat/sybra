package sybra

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestGetSettings_ExposesExpandedSections(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.persisted.GitHub.Enabled = true
	svc.persisted.Monitor.Enabled = true
	svc.persisted.Triage.PollSeconds = 45
	svc.persisted.ProjectTypes = []string{"pet"}
	writeConfigYAML(t, cfgPath, svc.persisted)

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

	explanations, err := svc.GetPathExplanations()
	if err != nil {
		t.Fatalf("GetPathExplanations: %v", err)
	}
	var githubEnabled *ConfigPathExplanation
	for i := range explanations {
		if explanations[i].Descriptor.RuntimePath == "github.enabled" {
			githubEnabled = &explanations[i]
			break
		}
	}
	if githubEnabled == nil {
		t.Fatal("github.enabled explanation missing")
	}
	if githubEnabled.ReloadPolicy != configPolicyRestart {
		t.Fatalf("github.enabled reload = %q, want %q", githubEnabled.ReloadPolicy, configPolicyRestart)
	}
	if !githubEnabled.Intent.Declared {
		t.Fatalf("github.enabled intent = %+v, want declared", githubEnabled.Intent)
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

func TestGetDefaultSettings_UsesRenamedGuardrailJSONFields(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	raw, err := json.Marshal(svc.GetDefaultSettings())
	if err != nil {
		t.Fatalf("Marshal(GetDefaultSettings): %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal(GetDefaultSettings): %v", err)
	}
	agent, ok := payload["agent"].(map[string]any)
	if !ok {
		t.Fatalf("agent payload type = %T, want object", payload["agent"])
	}
	if _, ok := agent["postResultCostUsd"]; !ok {
		t.Fatalf("agent JSON missing postResultCostUsd: %s", raw)
	}
	if _, ok := agent["maxAssistantEvents"]; !ok {
		t.Fatalf("agent JSON missing maxAssistantEvents: %s", raw)
	}
	if _, ok := agent["maxCostUsd"]; ok {
		t.Fatalf("agent JSON still exposes legacy maxCostUsd: %s", raw)
	}
	if _, ok := agent["maxTurns"]; ok {
		t.Fatalf("agent JSON still exposes legacy maxTurns: %s", raw)
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

func TestUpdateSettings_PatchesOnlyChangedLeafAndPreservesComments(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	raw := strings.Join([]string{
		"# operator header",
		"agent:",
		"  # keep this agent comment",
		"  provider: claude",
		"  max_concurrent: 3 # keep this inline comment",
		"webhook:",
		"  # keep this webhook comment",
		"  secret: keep-me",
		"",
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc.cfg = cfg
	svc.persisted = cloneConfig(cfg)

	settings := svc.GetSettings()
	settings.Agent.MaxConcurrent = 9

	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	for _, want := range []string{
		"# operator header",
		"# keep this agent comment",
		"# keep this inline comment",
		"# keep this webhook comment",
		"provider: claude",
		"max_concurrent: 9 # keep this inline comment",
		"secret: keep-me",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("saved config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "max_assistant_events:") || strings.Contains(text, "allowed_origins:") {
		t.Fatalf("UpdateSettings materialized unrelated defaults:\n%s", text)
	}
}

func TestUpdateSettings_ResetToDefaultRemovesExplicitKey(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	raw := strings.Join([]string{
		"browser:",
		"  in_app: true",
		"agent:",
		"  provider: claude",
		"",
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc.cfg = cfg
	svc.persisted = cloneConfig(cfg)

	settings := svc.GetSettings()
	settings.Browser = svc.GetDefaultSettings().Browser

	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	if strings.Contains(text, "in_app:") || strings.Contains(text, "browser:") {
		t.Fatalf("default reset left explicit browser config behind:\n%s", text)
	}
	if !strings.Contains(text, "provider: claude") {
		t.Fatalf("reset removed unrelated config:\n%s", text)
	}
}

func TestUpdateSettings_UpdatesDurationAliasInPlace(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	raw := strings.Join([]string{
		"schema_version: 2",
		"agent:",
		"  provider: claude",
		"  bash_timeout: 2m # keep this inline comment",
		"",
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc.cfg = cfg
	svc.persisted = cloneConfig(cfg)

	if cfg.Agent.BashTimeoutSeconds != 120 {
		t.Fatalf("alias not resolved: bash_timeout_seconds = %d, want 120", cfg.Agent.BashTimeoutSeconds)
	}

	settings := svc.GetSettings()
	settings.Agent.BashTimeoutSeconds = 300

	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	if strings.Contains(text, "bash_timeout_seconds") {
		t.Fatalf("patch wrote a conflicting legacy key beside the alias:\n%s", text)
	}
	if !strings.Contains(text, "bash_timeout: 300s") {
		t.Fatalf("alias entry not updated in place:\n%s", text)
	}
	if !strings.Contains(text, "# keep this inline comment") {
		t.Fatalf("alias inline comment dropped:\n%s", text)
	}

	// A mixed alias+legacy file would be rejected on reload; the patch must stay loadable.
	reloaded, err := config.Load()
	if err != nil {
		t.Fatalf("patched config no longer loads: %v", err)
	}
	if reloaded.Agent.BashTimeoutSeconds != 300 {
		t.Fatalf("reloaded bash_timeout_seconds = %d, want 300", reloaded.Agent.BashTimeoutSeconds)
	}
}

func TestUpdateSettings_KeepsNamespacedV2Layout(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	raw := strings.Join([]string{
		"schema_version: 2",
		"execution:",
		"  agent:",
		"    provider: claude",
		"    bash_timeout: 2m",
		"",
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc.cfg = cfg
	svc.persisted = cloneConfig(cfg)

	settings := svc.GetSettings()
	settings.Agent.MaxConcurrent = 7
	settings.Agent.BashTimeoutSeconds = 300

	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	if regexp.MustCompile(`(?m)^agent:`).MatchString(text) {
		t.Fatalf("settings patch fell back to flat top-level agent block:\n%s", text)
	}
	for _, want := range []string{
		"execution:",
		"  agent:",
		"    provider: claude",
		"    bash_timeout: 300s",
		"    max_concurrent: 7",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("saved namespaced config missing %q:\n%s", want, text)
		}
	}
}

func TestUpdateSettings_ResetToDefaultRemovesDurationAlias(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	raw := strings.Join([]string{
		"schema_version: 2",
		"agent:",
		"  provider: claude",
		"  bash_timeout: 2m",
		"",
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc.cfg = cfg
	svc.persisted = cloneConfig(cfg)

	settings := svc.GetSettings()
	settings.Agent.BashTimeoutSeconds = svc.GetDefaultSettings().Agent.BashTimeoutSeconds

	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	if strings.Contains(text, "bash_timeout") {
		t.Fatalf("reset to default left a duration entry behind:\n%s", text)
	}
	if !strings.Contains(text, "provider: claude") {
		t.Fatalf("reset removed unrelated config:\n%s", text)
	}
	if _, err := config.Load(); err != nil {
		t.Fatalf("patched config no longer loads: %v", err)
	}
}

func TestSparseConfigSequence_RawSaveThenSettingsEditThenResetStaysSparse(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	raw := strings.Join([]string{
		"schema_version: 2",
		"# top comment",
		"agent:",
		"  # keep me",
		"  provider: codex",
		"",
	}, "\n")
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AuthToken == "" {
		t.Fatal("expected generated server auth token in memory")
	}
	tokenPath := filepath.Join(filepath.Dir(cfgPath), "server_auth_token")
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("expected generated server_auth_token file: %v", err)
	}
	svc.cfg = cfg
	svc.persisted = cloneConfig(cfg)

	editedRaw := strings.Join([]string{
		"schema_version: 2",
		"# top comment",
		"agent:",
		"  # keep me",
		"  provider: claude",
		"",
	}, "\n")
	if err := svc.SaveRawConfig(editedRaw); err != nil {
		t.Fatalf("SaveRawConfig: %v", err)
	}

	assertSparse := func(stage, want string) {
		t.Helper()
		saved, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		got := string(saved)
		if got != want {
			t.Fatalf("%s saved config mismatch:\nwant:\n%s\ngot:\n%s", stage, want, got)
		}
		if strings.Contains(got, "auth_token") {
			t.Fatalf("%s materialized generated auth token into config.yaml:\n%s", stage, got)
		}
		if strings.Contains(got, "allowed_origins:") || strings.Contains(got, "logging:") {
			t.Fatalf("%s materialized unrelated defaults:\n%s", stage, got)
		}
	}

	assertSparse("after raw save", editedRaw)

	settings := svc.GetSettings()
	settings.Agent.MaxConcurrent = 7
	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	assertSparse("after settings edit", strings.Join([]string{
		"schema_version: 2",
		"# top comment",
		"agent:",
		"  # keep me",
		"  provider: claude",
		"  max_concurrent: 7",
		"",
	}, "\n"))

	settings = svc.GetSettings()
	settings.Agent.MaxConcurrent = svc.GetDefaultSettings().Agent.MaxConcurrent
	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings reset: %v", err)
	}
	assertSparse("after reset", editedRaw)
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

func TestSaveRawConfig_AcceptsLegacyGitHubPollingDurationAliases(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)

	raw := strings.Join([]string{
		"github:",
		"  enabled: true",
		"  poller_role: secondary",
		"  polling:",
		"    issues:",
		"      enabled: true",
		"      interval: 11m",
		"    sybra_prs:",
		"      enabled: true",
		"      active_interval: 2m",
		"      idle_interval: 9m",
		"    assigned_prs:",
		"      enabled: false",
		"      active_interval: 3m",
		"      idle_interval: 8m",
		"",
	}, "\n")

	if err := svc.SaveRawConfig(raw); err != nil {
		t.Fatalf("SaveRawConfig: %v", err)
	}
	got := svc.GetSettings().GitHub
	if got.PollerRole != "secondary" {
		t.Fatalf("PollerRole = %q, want secondary", got.PollerRole)
	}
	if got := got.Polling.Issues.IntervalSeconds; got != 11*60 {
		t.Fatalf("Issues.IntervalSeconds = %d, want %d", got, 11*60)
	}
	if got := got.Polling.SybraPRs.ActiveIntervalSeconds; got != 2*60 {
		t.Fatalf("SybraPRs.ActiveIntervalSeconds = %d, want %d", got, 2*60)
	}
	if got := got.Polling.SybraPRs.IdleIntervalSeconds; got != 9*60 {
		t.Fatalf("SybraPRs.IdleIntervalSeconds = %d, want %d", got, 9*60)
	}
	if got := got.Polling.AssignedPRs.ActiveIntervalSeconds; got != 3*60 {
		t.Fatalf("AssignedPRs.ActiveIntervalSeconds = %d, want %d", got, 3*60)
	}
	if got := got.Polling.AssignedPRs.IdleIntervalSeconds; got != 8*60 {
		t.Fatalf("AssignedPRs.IdleIntervalSeconds = %d, want %d", got, 8*60)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	savedText := string(saved)
	for _, want := range []string{
		"interval: 11m",
		"active_interval: 2m",
		"idle_interval: 9m",
		"active_interval: 3m",
		"idle_interval: 8m",
	} {
		if !strings.Contains(savedText, want) {
			t.Fatalf("saved config missing %q:\n%s", want, savedText)
		}
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

// TestSaveRawConfig_DoesNotMaterializeGeneratedTokenWhenAbsentFromFile
// reproduces a generated bearer token (resolved in-memory from the separate
// server_auth_token file, never written to config.yaml) surviving a raw save
// that never mentions server.auth_token. It must stay absent from disk.
func TestSaveRawConfig_DoesNotMaterializeGeneratedTokenWhenAbsentFromFile(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	if err := os.WriteFile(cfgPath, []byte("schema_version: 2\nagent:\n  provider: codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulates ensureServerAuthToken resolving a generated token from
	// server_auth_token at load time: present in memory, absent from the file.
	svc.cfg.Server.AuthToken = "generated-token"
	svc.persisted.Server.AuthToken = "generated-token"

	raw := "schema_version: 2\nagent:\n  provider: claude\n"
	if err := svc.SaveRawConfig(raw); err != nil {
		t.Fatalf("SaveRawConfig: %v", err)
	}

	saved, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	savedText := string(saved)
	if strings.Contains(savedText, "auth_token") {
		t.Fatalf("raw save materialized the generated token into config.yaml:\n%s", savedText)
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
