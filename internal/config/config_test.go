package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/abtest"
	yamlv3 "gopkg.in/yaml.v3"
)

func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()

	if cfg.Logging.Level != "info" {
		t.Errorf("Level = %q, want %q", cfg.Logging.Level, "info")
	}
	if cfg.Logging.MaxSizeMB != 50 {
		t.Errorf("MaxSizeMB = %d, want 50", cfg.Logging.MaxSizeMB)
	}
	if cfg.Logging.MaxFiles != 5 {
		t.Errorf("MaxFiles = %d, want 5", cfg.Logging.MaxFiles)
	}
	if cfg.Logging.Dir == "" {
		t.Error("Dir should not be empty")
	}
	if cfg.TasksDir == "" {
		t.Error("TasksDir should not be empty")
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("logging:\n  level: debug\n  max_size_mb: 10\n  max_files: 3\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Level = %q, want %q", cfg.Logging.Level, "debug")
	}
	if cfg.Logging.MaxSizeMB != 10 {
		t.Errorf("MaxSizeMB = %d, want 10", cfg.Logging.MaxSizeMB)
	}
	if cfg.Logging.MaxFiles != 3 {
		t.Errorf("MaxFiles = %d, want 3", cfg.Logging.MaxFiles)
	}
}

func TestInAppBrowserEnabled(t *testing.T) {
	t.Parallel()

	truthy, falsy := true, false

	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil config", nil, true},
		{"nil field", &Config{}, true},
		{"explicit true", &Config{Browser: BrowserConfig{InApp: &truthy}}, true},
		{"explicit false", &Config{Browser: BrowserConfig{InApp: &falsy}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.InAppBrowserEnabled(); got != tc.want {
				t.Errorf("InAppBrowserEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadProviderDefaultAndPersistedValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.Provider != "claude" {
		t.Fatalf("default provider = %q, want claude", cfg.Agent.Provider)
	}

	cfg.Agent.Provider = "codex"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Agent.Provider != "codex" {
		t.Fatalf("reloaded provider = %q, want codex", reloaded.Agent.Provider)
	}
}

// TestDefaultDispatchJitterAndInFlightCap locks in the jitter/soft-cap
// defaults: jitter defaults on (1000ms) to spread dispatch against a shared
// subscription's rate limit; the in-flight soft-cap stays off (0) so
// existing deployments upgrade with no behavior change there.
func TestDefaultDispatchJitterAndInFlightCap(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	if cfg.Agent.MaxConcurrent != 25 {
		t.Errorf("Agent.MaxConcurrent = %d, want 25", cfg.Agent.MaxConcurrent)
	}
	if cfg.Agent.DispatchJitterMs != 1000 {
		t.Errorf("Agent.DispatchJitterMs = %d, want 1000 (default enabled)", cfg.Agent.DispatchJitterMs)
	}
	if cfg.Providers.Limits.MaxInFlightPerProvider != 0 {
		t.Errorf("Providers.Limits.MaxInFlightPerProvider = %d, want 0 (disabled)", cfg.Providers.Limits.MaxInFlightPerProvider)
	}
}

// TestLoadPreservesDispatchJitterAndInFlightCapOverrides verifies both new
// knobs round-trip through YAML like the other Agent/Providers.Limits fields.
func TestLoadPreservesDispatchJitterAndInFlightCapOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	yamlDoc := []byte("agent:\n  dispatch_jitter_ms: 250\nproviders:\n  limits:\n    max_in_flight_per_provider: 3\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yamlDoc, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.DispatchJitterMs != 250 {
		t.Errorf("Agent.DispatchJitterMs = %d, want 250", cfg.Agent.DispatchJitterMs)
	}
	if cfg.Providers.Limits.MaxInFlightPerProvider != 3 {
		t.Errorf("Providers.Limits.MaxInFlightPerProvider = %d, want 3", cfg.Providers.Limits.MaxInFlightPerProvider)
	}
}

func TestLoadExperienceDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Experience.Enabled {
		t.Fatal("experience.enabled = true, want false")
	}
	if cfg.Experience.MaxRecords != 5 {
		t.Fatalf("experience.max_records = %d, want 5", cfg.Experience.MaxRecords)
	}
	if got := cfg.ExperiencesDir(); got != filepath.Join(dir, "experience") {
		t.Fatalf("ExperiencesDir() = %q, want under SYBRA_HOME", got)
	}
}

func TestLoadAutoUpdateDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("auto_update:\n  enabled: true\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AutoUpdate.Enabled {
		t.Fatal("auto_update.enabled = false, want true")
	}
	if cfg.AutoUpdate.Remote != "origin" {
		t.Fatalf("auto_update.remote = %q, want origin", cfg.AutoUpdate.Remote)
	}
	if cfg.AutoUpdate.Branch != "main" {
		t.Fatalf("auto_update.branch = %q, want main", cfg.AutoUpdate.Branch)
	}
	if cfg.AutoUpdate.Mode != "notify" {
		t.Fatalf("auto_update.mode = %q, want notify", cfg.AutoUpdate.Mode)
	}
	if cfg.AutoUpdate.PollSeconds != 300 {
		t.Fatalf("auto_update.poll_seconds = %d, want 300", cfg.AutoUpdate.PollSeconds)
	}
	if cfg.AutoUpdate.RestartDelaySeconds != 2 {
		t.Fatalf("auto_update.restart_delay_seconds = %d, want 2", cfg.AutoUpdate.RestartDelaySeconds)
	}
}

func TestLoadMonitorDispatchLimitDefaultsToAgentLimit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("agent:\n  max_concurrent: 10\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor.DispatchLimit != 10 {
		t.Fatalf("dispatch limit = %d, want 10", cfg.Monitor.DispatchLimit)
	}
}

func TestLoadMonitorDispatchLimitPreservesOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("agent:\n  max_concurrent: 10\nmonitor:\n  dispatch_limit: 4\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor.DispatchLimit != 4 {
		t.Fatalf("dispatch limit = %d, want 4", cfg.Monitor.DispatchLimit)
	}
}

func TestLoadMonitorPRGapGraceDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("monitor:\n  pr_gap_grace_minutes: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Monitor.PRGapGraceMinutes != 15 {
		t.Fatalf("PRGapGraceMinutes = %d, want 15", cfg.Monitor.PRGapGraceMinutes)
	}
}

func TestLoadHarnessEvolutionDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HarnessEvolve.Enabled {
		t.Fatal("harness evolution should default enabled")
	}
	if cfg.HarnessEvolve.IntervalHours != 24 {
		t.Fatalf("interval = %.0f, want 24", cfg.HarnessEvolve.IntervalHours)
	}
	if cfg.HarnessEvolve.LookbackHours != 168 {
		t.Fatalf("lookback = %.0f, want 168", cfg.HarnessEvolve.LookbackHours)
	}
	if cfg.HarnessEvolve.MinClusterSize != 2 {
		t.Fatalf("min cluster size = %d, want 2", cfg.HarnessEvolve.MinClusterSize)
	}
	if cfg.HarnessEvolve.Sink != "local-task" {
		t.Fatalf("sink = %q, want local-task", cfg.HarnessEvolve.Sink)
	}
}

func TestLoadHarnessEvolutionPreservesExplicitDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	yaml := []byte("harness_evolution:\n  enabled: false\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HarnessEvolve.Enabled {
		t.Fatal("explicit harness_evolution.enabled=false was not preserved")
	}
}

func TestLoadPromptLabDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Unlike harness evolution, prompt lab must default disabled: a config
	// predating this feature (or a fresh install) must not start filing
	// proposal tasks until an operator opts in.
	if cfg.PromptLab.Enabled {
		t.Fatal("prompt lab should default disabled")
	}
	if cfg.PromptLab.IntervalHours != 24 {
		t.Fatalf("interval = %.0f, want 24", cfg.PromptLab.IntervalHours)
	}
	if cfg.PromptLab.LookbackHours != 168 {
		t.Fatalf("lookback = %.0f, want 168", cfg.PromptLab.LookbackHours)
	}
	if cfg.PromptLab.MinSamples != 5 {
		t.Fatalf("min samples = %d, want 5", cfg.PromptLab.MinSamples)
	}
	if cfg.PromptLab.MinEffectSize != 0.15 {
		t.Fatalf("min effect size = %v, want 0.15", cfg.PromptLab.MinEffectSize)
	}
	if cfg.PromptLab.MaxProposalsPerRun != 3 {
		t.Fatalf("max proposals per run = %d, want 3", cfg.PromptLab.MaxProposalsPerRun)
	}
}

func TestLoadPromptLabPreservesExplicitEnabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	yaml := []byte("prompt_lab:\n  enabled: true\n  min_samples: 10\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PromptLab.Enabled {
		t.Fatal("explicit prompt_lab.enabled=true was not preserved")
	}
	if cfg.PromptLab.MinSamples != 10 {
		t.Fatalf("explicit prompt_lab.min_samples=10 was not preserved, got %d", cfg.PromptLab.MinSamples)
	}
}

func TestHumanReviewSybraBugAction(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{name: "empty defaults to file_issue", yaml: "human_review:\n  enabled: true\n", want: HumanReviewSybraBugActionFileIssue},
		{name: "note only", yaml: "human_review:\n  sybra_bug_action: note_only\n", want: HumanReviewSybraBugActionNoteOnly},
		{name: "block only", yaml: "human_review:\n  sybra_bug_action: block_only\n", want: HumanReviewSybraBugActionBlockOnly},
		{name: "local task", yaml: "human_review:\n  sybra_bug_action: local_task\n", want: HumanReviewSybraBugActionLocalTask},
		{name: "invalid falls back", yaml: "human_review:\n  sybra_bug_action: nope\n", want: HumanReviewSybraBugActionFileIssue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("SYBRA_HOME", dir)
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.HumanReviewSybraBugAction(); got != tc.want {
				t.Fatalf("HumanReviewSybraBugAction() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReviewHoldDefaults(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		wantEnabled bool
		wantMode    string
		wantNit     int
	}{
		{
			name:        "absent block is disabled",
			yaml:        "logging:\n  level: info\n",
			wantEnabled: false,
			wantMode:    ReviewHoldModePush, // accessor falls back even when off
			wantNit:     DefaultReviewHoldNitMaxLines,
		},
		{
			name:        "enabled with empty mode defaults to push and nit ceiling",
			yaml:        "review_hold:\n  enabled: true\n",
			wantEnabled: true,
			wantMode:    ReviewHoldModePush,
			wantNit:     DefaultReviewHoldNitMaxLines,
		},
		{
			name:        "invalid mode falls back to push",
			yaml:        "review_hold:\n  enabled: true\n  mode: bogus\n",
			wantEnabled: true,
			wantMode:    ReviewHoldModePush,
			wantNit:     DefaultReviewHoldNitMaxLines,
		},
		{
			name:        "push_nits mode preserves an explicit threshold",
			yaml:        "review_hold:\n  enabled: true\n  mode: push_nits\n  nit_max_lines: 42\n",
			wantEnabled: true,
			wantMode:    ReviewHoldModePushNits,
			wantNit:     42,
		},
		{
			name:        "hold mode is preserved",
			yaml:        "review_hold:\n  enabled: true\n  mode: hold\n",
			wantEnabled: true,
			wantMode:    ReviewHoldModeHold,
			wantNit:     DefaultReviewHoldNitMaxLines,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("SYBRA_HOME", dir)
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.ReviewHoldEnabled(); got != tc.wantEnabled {
				t.Errorf("ReviewHoldEnabled() = %v, want %v", got, tc.wantEnabled)
			}
			if got := cfg.ReviewHoldMode(); got != tc.wantMode {
				t.Errorf("ReviewHoldMode() = %q, want %q", got, tc.wantMode)
			}
			if got := cfg.ReviewHoldNitMaxLines(); got != tc.wantNit {
				t.Errorf("ReviewHoldNitMaxLines() = %d, want %d", got, tc.wantNit)
			}
		})
	}
}

func TestLoadWatchdogDefaults(t *testing.T) {
	tests := []struct {
		name          string
		yaml          string
		wantEnabled   bool
		wantThreshold int
		wantModel     string
	}{
		{
			name:          "missing block keeps always-on defaults",
			yaml:          "agent:\n  max_concurrent: 10\n",
			wantEnabled:   true,
			wantThreshold: 6,
			wantModel:     "claude-haiku-4-5-20251001",
		},
		{
			name:          "explicit loop_threshold 0 disables loop detection",
			yaml:          "watchdog:\n  enabled: true\n  loop_threshold: 0\n",
			wantEnabled:   true,
			wantThreshold: 0,
			wantModel:     "claude-haiku-4-5-20251001",
		},
		{
			name:          "explicit overrides preserved",
			yaml:          "watchdog:\n  enabled: false\n  loop_threshold: 3\n  model: sonnet\n",
			wantEnabled:   false,
			wantThreshold: 3,
			wantModel:     "sonnet",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("SYBRA_HOME", dir)
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Watchdog.Enabled != tc.wantEnabled {
				t.Errorf("Enabled = %v, want %v", cfg.Watchdog.Enabled, tc.wantEnabled)
			}
			if cfg.Watchdog.LoopThreshold != tc.wantThreshold {
				t.Errorf("LoopThreshold = %d, want %d", cfg.Watchdog.LoopThreshold, tc.wantThreshold)
			}
			if cfg.Watchdog.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", cfg.Watchdog.Model, tc.wantModel)
			}
		})
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	t.Setenv("SYBRA_LOG_LEVEL", "error")
	t.Setenv("SYBRA_LOG_DIR", "/tmp/test-logs")
	t.Setenv("SYBRA_TASKS_DIR", "/tmp/test-tasks")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Level != "error" {
		t.Errorf("Level = %q, want %q", cfg.Logging.Level, "error")
	}
	if cfg.Logging.Dir != "/tmp/test-logs" {
		t.Errorf("Dir = %q, want %q", cfg.Logging.Dir, "/tmp/test-logs")
	}
	if cfg.TasksDir != "/tmp/test-tasks" {
		t.Errorf("TasksDir = %q, want %q", cfg.TasksDir, "/tmp/test-tasks")
	}
}

func TestSlogLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo},
		{"", slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			t.Parallel()
			cfg := &LoggingConfig{Level: tt.level}
			if got := cfg.SlogLevel(); got != tt.want {
				t.Errorf("SlogLevel(%q) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestLoadMissingConfigCreatesDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Level = %q, want %q", cfg.Logging.Level, "info")
	}

	// config.yaml should have been created
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil {
		t.Errorf("config.yaml not created: %v", err)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(":{bad yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadEmptyDirFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("logging:\n  dir: \"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Dir == "" {
		t.Error("Dir should fall back to default, not be empty")
	}
}

func TestAllowsProjectType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		cfg   *Config
		ptype string
		want  bool
	}{
		{"nil config allows all", nil, "pet", true},
		{"empty list allows all", &Config{}, "pet", true},
		{"empty list allows work", &Config{}, "work", true},
		{"pet-only allows pet", &Config{ProjectTypes: []string{"pet"}}, "pet", true},
		{"pet-only blocks work", &Config{ProjectTypes: []string{"pet"}}, "work", false},
		{"work-only blocks pet", &Config{ProjectTypes: []string{"work"}}, "pet", false},
		{"both allows pet", &Config{ProjectTypes: []string{"pet", "work"}}, "pet", true},
		{"both allows work", &Config{ProjectTypes: []string{"pet", "work"}}, "work", true},
		{"unknown type blocked", &Config{ProjectTypes: []string{"pet"}}, "other", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cfg.AllowsProjectType(tt.ptype); got != tt.want {
				t.Errorf("AllowsProjectType(%q) = %v, want %v", tt.ptype, got, tt.want)
			}
		})
	}
}

func TestDefaultGitHubEnabled(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	if !cfg.GitHub.Enabled {
		t.Error("default GitHub.Enabled should be true for backward compat")
	}
	if cfg.GitHub.NativeAutoMerge {
		t.Error("default GitHub.NativeAutoMerge should be false (kill-switch, opt-in)")
	}
}

func TestLoadGitHubNativeAutoMerge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("github:\n  native_auto_merge: true\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GitHub.NativeAutoMerge {
		t.Error("NativeAutoMerge = false, want true after round-tripping through yaml")
	}
}

func TestDefaultGitHubAutoResolveCleanMerges(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	if cfg.GitHub.AutoResolveCleanMerges {
		t.Error("default GitHub.AutoResolveCleanMerges should be false (kill-switch, opt-in)")
	}
}

func TestLoadGitHubAutoResolveCleanMerges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	yaml := []byte("github:\n  auto_resolve_clean_merges: true\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GitHub.AutoResolveCleanMerges {
		t.Error("AutoResolveCleanMerges = false, want true after round-tripping through yaml")
	}
}

func TestDefaultRequirePermissions(t *testing.T) {
	t.Parallel()
	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil config", nil, true},
		{"nil field", &Config{}, true},
		{"explicit true", &Config{Agent: AgentDefaults{RequirePermissions: boolPtr(true)}}, true},
		{"explicit false", &Config{Agent: AgentDefaults{RequirePermissions: boolPtr(false)}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cfg.DefaultRequirePermissions(); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadMigratesStaleSkillsDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	// Simulate the pre-cdb6dc5 default that users still have persisted.
	stale := filepath.Join(dir, "skills")
	yaml := []byte("skills_dir: " + stale + "\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".claude", "skills")
	if cfg.SkillsDir != want {
		t.Fatalf("SkillsDir = %q, want %q (migration should silently retarget the old default)", cfg.SkillsDir, want)
	}
}

func TestLoadPreservesCustomSkillsDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	custom := "/tmp/my-custom-skills"
	yaml := []byte("skills_dir: " + custom + "\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), yaml, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SkillsDir != custom {
		t.Fatalf("SkillsDir = %q, want %q (only the stale default should be migrated)", cfg.SkillsDir, custom)
	}
}

func TestHomeDirDefault(t *testing.T) {
	t.Setenv("SYBRA_HOME", "")

	dir := HomeDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".sybra")
	if dir != want {
		t.Errorf("HomeDir() = %q, want %q", dir, want)
	}
}

func TestHomeDirOverride(t *testing.T) {
	t.Setenv("SYBRA_HOME", "/custom/sybra")

	dir := HomeDir()
	if dir != "/custom/sybra" {
		t.Errorf("HomeDir() = %q, want %q", dir, "/custom/sybra")
	}
}

func TestPathsUnderHomeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)

	if got := configPath(); got != filepath.Join(dir, "config.yaml") {
		t.Errorf("configPath() = %q, want under %q", got, dir)
	}
	if got := defaultLogDir(); got != filepath.Join(dir, "logs") {
		t.Errorf("defaultLogDir() = %q, want under %q", got, dir)
	}
	if got := defaultTasksDir(); got != filepath.Join(dir, "tasks") {
		t.Errorf("defaultTasksDir() = %q, want under %q", got, dir)
	}
	if got := ArtifactsDir(); got != filepath.Join(dir, "artifacts") {
		t.Errorf("ArtifactsDir() = %q, want under %q", got, dir)
	}
}

func TestDefaultLogRetentionDays(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *Config
		want int
	}{
		{"nil config → 14", nil, 14},
		{"unset → 14", &Config{}, 14},
		{"explicit 7 → 7", &Config{Agent: AgentDefaults{LogRetentionDays: 7}}, 7},
		{"negative disables (sentinel preserved)", &Config{Agent: AgentDefaults{LogRetentionDays: -1}}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.DefaultLogRetentionDays(); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRetryWatchdog(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *Config
		want int
	}{
		{"nil config → default 30", nil, DefaultRetryWatchdog},
		{"unset → default 30", &Config{}, DefaultRetryWatchdog},
		{"explicit 45 → 45", &Config{Agent: AgentDefaults{RetryWatchdog: 45}}, 45},
		{"negative disables (returns 0)", &Config{Agent: AgentDefaults{RetryWatchdog: -1}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.RetryWatchdog(); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// oldShapeABTestingExperiments returns the pre-reconcile shape: the original
// single shared "code-author-cheap" bracket (hand-tuned claude weight 99, a
// value neither the old nor new DefaultConfig would ever produce, so a
// recovered backup is unambiguously distinguishable), no builtin_version
// stamp, plus a user-authored experiment that must survive reconcile intact.
func oldShapeABTestingExperiments() []abtest.Experiment {
	enabled := true
	return []abtest.Experiment{
		{
			ID:             "code-author-cheap",
			Enabled:        &enabled,
			AssignmentUnit: "stage",
			Bracket:        "cheap",
			Roles:          []string{"implementation", "test-runner", "fix-review", "pr-fix"},
			Variants: []abtest.Variant{
				{ID: "claude-sonnet", Provider: "claude", Model: "sonnet", Tier: "cheap", Weight: 99},
				{ID: "codex-gpt-5.4", Provider: "codex", Model: "gpt-5.4", Tier: "cheap", Weight: 2},
			},
		},
		{
			ID:             "my-custom-experiment",
			Enabled:        &enabled,
			AssignmentUnit: "stage",
			Roles:          []string{"implementation"},
			Variants: []abtest.Variant{
				{ID: "custom-v1", Provider: "claude", Model: "sonnet", Weight: 1},
			},
		},
	}
}

func writeOldShapeConfig(t *testing.T, dir string) {
	t.Helper()
	enabled := true
	cfg := &Config{
		ABTesting: abtest.Config{
			Enabled:              &enabled,
			MinSamplesPerVariant: 20,
			Experiments:          oldShapeABTestingExperiments(),
			// BuiltinVersion deliberately left at zero: simulates a config
			// persisted before builtin_version existed.
		},
	}
	data, err := yamlv3.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadReconcilesStaleBuiltinABExperiments proves an existing persisted
// config (predating the code-author-cheap/-maintenance-cheap split) adopts
// the new built-in experiments on Load, not only fresh installs.
func TestLoadReconcilesStaleBuiltinABExperiments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	writeOldShapeConfig(t, dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ABTesting.BuiltinVersionValue(); got != abtest.CurrentBuiltinVersion {
		t.Fatalf("BuiltinVersion = %d, want %d", got, abtest.CurrentBuiltinVersion)
	}

	byID := make(map[string]abtest.Experiment, len(cfg.ABTesting.Experiments))
	for _, exp := range cfg.ABTesting.Experiments {
		byID[exp.ID] = exp
	}

	if _, ok := byID["code-author-maintenance-cheap"]; !ok {
		t.Fatal("code-author-maintenance-cheap not adopted by reconcile")
	}
	authorCheap, ok := byID["code-author-cheap"]
	if !ok {
		t.Fatal("code-author-cheap missing after reconcile")
	}
	for _, v := range authorCheap.Variants {
		if v.ID == "claude-sonnet" && v.Weight == 99 {
			t.Fatal("code-author-cheap still carries the stale hand-tuned weight; reconcile did not replace it")
		}
	}
	if len(authorCheap.Roles) != 1 || authorCheap.Roles[0] != "implementation" {
		t.Fatalf("code-author-cheap roles = %v, want [implementation] (role split did not land)", authorCheap.Roles)
	}

	if _, ok := byID["my-custom-experiment"]; !ok {
		t.Fatal("user-authored experiment my-custom-experiment was dropped by reconcile")
	}
}

// TestLoadReconcilePersistsBackupBeforeOverwrite proves a one-generation
// backup of the prior experiment list is written to disk before the stale
// same-ID built-in is overwritten, so a hand-tuned built-in is recoverable.
func TestLoadReconcilePersistsBackupBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	writeOldShapeConfig(t, dir)

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(dir, "config.ab_testing.backup.v0.yaml")
	data, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup file not written: %v", err)
	}
	var backup struct {
		PriorBuiltinVersion int                 `yaml:"prior_builtin_version"`
		Experiments         []abtest.Experiment `yaml:"experiments"`
	}
	if err := yamlv3.Unmarshal(data, &backup); err != nil {
		t.Fatalf("backup file unreadable: %v", err)
	}
	if backup.PriorBuiltinVersion != 0 {
		t.Fatalf("PriorBuiltinVersion = %d, want 0", backup.PriorBuiltinVersion)
	}
	var recovered *abtest.Experiment
	for i := range backup.Experiments {
		if backup.Experiments[i].ID == "code-author-cheap" {
			recovered = &backup.Experiments[i]
		}
	}
	if recovered == nil {
		t.Fatal("backup does not contain the prior code-author-cheap experiment")
	}
	found := false
	for _, v := range recovered.Variants {
		if v.ID == "claude-sonnet" && v.Weight == 99 {
			found = true
		}
	}
	if !found {
		t.Fatal("backup lost the hand-tuned claude-sonnet weight — same-ID built-in is not recoverable")
	}
}

// TestLoadReconcilePersistsToDisk proves the reconciled config is written
// back to config.yaml immediately, so the refresh survives a process
// restart rather than living only in the in-memory Load() result.
func TestLoadReconcilePersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	writeOldShapeConfig(t, dir)

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Config
	if err := yamlv3.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if got := onDisk.ABTesting.BuiltinVersionValue(); got != abtest.CurrentBuiltinVersion {
		t.Fatalf("persisted BuiltinVersion = %d, want %d", got, abtest.CurrentBuiltinVersion)
	}
	foundCustom := false
	for _, exp := range onDisk.ABTesting.Experiments {
		if exp.ID == "my-custom-experiment" {
			foundCustom = true
		}
	}
	if !foundCustom {
		t.Fatal("persisted config.yaml lost the user-authored experiment")
	}
}

// TestLoadDoesNotReconcileUpToDateBuiltins proves a config already stamped at
// the current builtin version is left alone — no repeat backup/rewrite work
// on every Load (e.g. every hot reload).
func TestLoadDoesNotReconcileUpToDateBuiltins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	enabled := true
	cfg := &Config{
		ABTesting: abtest.Config{
			Enabled:              &enabled,
			MinSamplesPerVariant: 20,
			BuiltinVersion: func() *int {
				v := abtest.CurrentBuiltinVersion
				return &v
			}(),
			Experiments: abtest.DefaultConfig().Experiments,
		},
	}
	data, err := yamlv3.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "config.ab_testing.backup.v0.yaml")); !os.IsNotExist(err) {
		t.Fatalf("backup file should not be written when already at current builtin version, stat err = %v", err)
	}
}

func TestLoadNoPersistLeavesStaleConfigUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	writeOldShapeConfig(t, dir)

	before, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadNoPersist()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ABTesting.BuiltinVersionValue(); got != abtest.CurrentBuiltinVersion {
		t.Fatalf("BuiltinVersion = %d, want %d", got, abtest.CurrentBuiltinVersion)
	}

	after, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("LoadNoPersist rewrote config.yaml")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.ab_testing.backup.v0.yaml")); !os.IsNotExist(err) {
		t.Fatalf("LoadNoPersist should not write backup, stat err = %v", err)
	}
}

func TestLoadReconcileKeepsVersionedBackupsPerPriorBuiltinVersion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYBRA_HOME", dir)
	writeOldShapeConfig(t, dir)

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	firstBackup, err := os.ReadFile(filepath.Join(dir, "config.ab_testing.backup.v0.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(data), "builtin_version: 2", "builtin_version: 1", 1)
	if rewritten == string(data) {
		t.Fatal("failed to downgrade builtin_version in persisted config")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	secondBackup, err := os.ReadFile(filepath.Join(dir, "config.ab_testing.backup.v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stillFirstBackup, err := os.ReadFile(filepath.Join(dir, "config.ab_testing.backup.v0.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stillFirstBackup, firstBackup) {
		t.Fatal("v0 backup was overwritten by a later reconcile")
	}
	if len(secondBackup) == 0 {
		t.Fatal("v1 backup should not be empty")
	}
}
