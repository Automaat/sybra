package sybra

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/notification"
	"gopkg.in/yaml.v3"
)

// setupConfigSvc creates a ConfigService wired to a temp SYBRA_HOME.
// Returns the service and the path to config.yaml for mutation.
func setupConfigSvc(t *testing.T) (svc *ConfigService, cfgPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	// Write seed config; Load applies all defaults so s.cfg matches what
	// ReloadFromDisk will produce (e.g. Todoist.PollSeconds = 120).
	seed := config.DefaultConfig()
	seed.Agent.MaxConcurrent = 3
	seed.Agent.Provider = "claude"
	seed.Agent.MaxCostUSD = 5.0
	seed.Agent.MaxTurns = 150
	seed.Logging.Level = "info"
	seed.Logging.MaxSizeMB = 50
	seed.Logging.MaxFiles = 5
	seed.Audit.RetentionDays = 30

	cfgPath = filepath.Join(home, "config.yaml")
	writeConfigYAML(t, cfgPath, seed)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	emit := func(string, any) {}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	logDir := filepath.Join(home, "logs")
	mgr := agent.NewManager(context.Background(), emit, logger, logDir)
	mgr.SetMaxConcurrent(cfg.Agent.MaxConcurrent)
	mgr.SetDefaultProvider(cfg.Agent.Provider)
	mgr.SetGuardrails(agent.Guardrails{MaxCostUSD: cfg.Agent.MaxCostUSD, MaxTurns: cfg.Agent.MaxTurns})

	notifier := notification.New(emit)

	svc = &ConfigService{
		cfg:      cfg,
		logLevel: logLevel,
		notifier: notifier,
		agents:   mgr,
		logger:   logger,
	}
	return
}

func writeConfigYAML(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReloadFromDisk_MaxConcurrent(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	// Rewrite config with higher concurrency
	next := *svc.cfg
	next.Agent.MaxConcurrent = 8
	writeConfigYAML(t, cfgPath, &next)

	hot, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(hot, "agent.max_concurrent") {
		t.Errorf("expected agent.max_concurrent in hot, got %v", hot)
	}
	if got := svc.agents.RunningCount(); got < 0 {
		t.Error("unexpected RunningCount")
	}
	// Verify manager updated (can spin agents up to new limit)
	if svc.cfg.Agent.MaxConcurrent != 8 {
		t.Errorf("cfg.Agent.MaxConcurrent = %d, want 8", svc.cfg.Agent.MaxConcurrent)
	}
}

func TestReloadFromDisk_Guardrails(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	next := *svc.cfg
	next.Agent.MaxCostUSD = 20.0
	next.Agent.MaxTurns = 300
	writeConfigYAML(t, cfgPath, &next)

	hot, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(hot, "agent.max_cost_usd") {
		t.Errorf("expected agent.max_cost_usd in hot, got %v", hot)
	}
	g := svc.agents.Guardrails()
	if g.MaxCostUSD != 20.0 {
		t.Errorf("Guardrails.MaxCostUSD = %v, want 20.0", g.MaxCostUSD)
	}
	if g.MaxTurns != 300 {
		t.Errorf("Guardrails.MaxTurns = %d, want 300", g.MaxTurns)
	}
}

func TestReloadFromDisk_Provider(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	next := *svc.cfg
	next.Agent.Provider = "codex"
	writeConfigYAML(t, cfgPath, &next)

	hot, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(hot, "agent.provider") {
		t.Errorf("expected agent.provider in hot, got %v", hot)
	}
	if got := svc.agents.DefaultProvider(); got != "codex" {
		t.Errorf("DefaultProvider = %q, want codex", got)
	}
}

func TestReloadFromDisk_LogLevel(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	next := *svc.cfg
	next.Logging.Level = "debug"
	writeConfigYAML(t, cfgPath, &next)

	hot, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(hot, "logging.level") {
		t.Errorf("expected logging.level in hot, got %v", hot)
	}
	if svc.logLevel.Level() != slog.LevelDebug {
		t.Errorf("logLevel = %v, want Debug", svc.logLevel.Level())
	}
}

func TestReloadFromDisk_InvalidYAML(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	if err := os.WriteFile(cfgPath, []byte(":::invalid yaml{{{\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origMax := svc.cfg.Agent.MaxConcurrent
	_, err := svc.ReloadFromDisk()
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if svc.cfg.Agent.MaxConcurrent != origMax {
		t.Errorf("cfg mutated on error: got %d, want %d", svc.cfg.Agent.MaxConcurrent, origMax)
	}
}

func TestReloadFromDisk_ValidationError(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	next := *svc.cfg
	next.Logging.Level = "verbose" // invalid
	writeConfigYAML(t, cfgPath, &next)

	origLevel := svc.cfg.Logging.Level
	_, err := svc.ReloadFromDisk()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if svc.cfg.Logging.Level != origLevel {
		t.Errorf("cfg.Logging.Level mutated on validation error")
	}
}

func TestReloadFromDisk_RestartRequiredWarned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	seed := config.DefaultConfig()
	seed.Logging.Level = "info"
	seed.Logging.MaxSizeMB = 50
	seed.Logging.MaxFiles = 5
	seed.Audit.RetentionDays = 30

	cfgPath := filepath.Join(home, "config.yaml")
	writeConfigYAML(t, cfgPath, seed)

	// Use Load() so s.cfg matches what ReloadFromDisk will see.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Capture log records
	var records []slog.Record
	handler := &recordHandler{records: &records}
	logger := slog.New(handler)

	emit := func(string, any) {}
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	logDir := filepath.Join(home, "logs")
	mgr := agent.NewManager(context.Background(), emit, logger, logDir)
	mgr.SetMaxConcurrent(cfg.Agent.MaxConcurrent)
	mgr.SetDefaultProvider(cfg.Agent.Provider)
	mgr.SetGuardrails(agent.Guardrails{MaxCostUSD: cfg.Agent.MaxCostUSD, MaxTurns: cfg.Agent.MaxTurns})
	notifier := notification.New(emit)

	svc := &ConfigService{
		cfg:      cfg,
		logLevel: logLevel,
		notifier: notifier,
		agents:   mgr,
		logger:   logger,
	}

	// Change a restart-required field
	next := *cfg
	next.Providers.HealthCheck.IntervalSeconds = 600
	writeConfigYAML(t, cfgPath, &next)

	hot, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if len(hot) != 0 {
		t.Errorf("expected no hot keys, got %v", hot)
	}

	// Check that restart_required was logged
	found := false
	for _, r := range records {
		if r.Message == "config.reload.restart_required" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected config.reload.restart_required warning, got none")
	}

	// Verify s.cfg updated to disk value (prevents repeated warnings)
	if svc.cfg.Providers.HealthCheck.IntervalSeconds != 600 {
		t.Error("s.cfg not updated with restart-required field after reload")
	}

	// Second reload with same content: no new warnings
	prevCount := len(records)
	hot2, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("second ReloadFromDisk: %v", err)
	}
	if len(hot2) != 0 {
		t.Errorf("second reload: expected no hot keys, got %v", hot2)
	}
	warnCount := 0
	for _, r := range records[prevCount:] {
		if r.Message == "config.reload.restart_required" {
			warnCount++
		}
	}
	if warnCount != 0 {
		t.Errorf("second reload with same content emitted %d restart warnings, want 0", warnCount)
	}
}

func TestReloadFromDisk_NoFeedbackLoop(t *testing.T) {

	// UpdateSettings saves to disk; watcher fires ReloadFromDisk; diff should
	// be empty since disk now matches in-memory cfg.
	svc, _ := setupConfigSvc(t)

	// UpdateSettings mutates cfg and saves
	settings := AppSettings{
		Agent:        svc.cfg.Agent,
		Notification: svc.cfg.Notification,
		Orchestrator: svc.cfg.Orchestrator,
		Logging: LoggingSettings{
			Level:     "warn",
			MaxSizeMB: svc.cfg.Logging.MaxSizeMB,
			MaxFiles:  svc.cfg.Logging.MaxFiles,
		},
		Audit:     svc.cfg.Audit,
		Todoist:   svc.cfg.Todoist,
		Renovate:  svc.cfg.Renovate,
		Providers: svc.cfg.Providers,
	}
	settings.Agent.MaxConcurrent = 3 // ensure valid
	if err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// Simulate watcher-triggered reload — disk now matches in-memory
	hot, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if len(hot) != 0 {
		t.Errorf("feedback loop: expected empty hot keys after UpdateSettings+Reload, got %v", hot)
	}
}

// recordHandler captures log records for assertion in tests.
type recordHandler struct {
	records *[]slog.Record
	slog.Handler
}

func (h *recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}
func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }
