package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
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
	t.Setenv("SYNAPSE_HOME", dir)

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

func TestLoadProviderDefaultAndPersistedValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYNAPSE_HOME", dir)

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

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("SYNAPSE_HOME", t.TempDir())
	t.Setenv("SYNAPSE_LOG_LEVEL", "error")
	t.Setenv("SYNAPSE_LOG_DIR", "/tmp/test-logs")
	t.Setenv("SYNAPSE_TASKS_DIR", "/tmp/test-tasks")

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
	t.Setenv("SYNAPSE_HOME", dir)

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
	t.Setenv("SYNAPSE_HOME", dir)

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
	t.Setenv("SYNAPSE_HOME", dir)

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
	t.Setenv("SYNAPSE_HOME", dir)

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
	t.Setenv("SYNAPSE_HOME", dir)

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
	t.Setenv("SYNAPSE_HOME", "")

	dir := HomeDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".synapse")
	if dir != want {
		t.Errorf("HomeDir() = %q, want %q", dir, want)
	}
}

func TestHomeDirOverride(t *testing.T) {
	t.Setenv("SYNAPSE_HOME", "/custom/synapse")

	dir := HomeDir()
	if dir != "/custom/synapse" {
		t.Errorf("HomeDir() = %q, want %q", dir, "/custom/synapse")
	}
}

func TestPathsUnderHomeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SYNAPSE_HOME", dir)

	if got := configPath(); got != filepath.Join(dir, "config.yaml") {
		t.Errorf("configPath() = %q, want under %q", got, dir)
	}
	if got := defaultLogDir(); got != filepath.Join(dir, "logs") {
		t.Errorf("defaultLogDir() = %q, want under %q", got, dir)
	}
	if got := defaultTasksDir(); got != filepath.Join(dir, "tasks") {
		t.Errorf("defaultTasksDir() = %q, want under %q", got, dir)
	}
}
