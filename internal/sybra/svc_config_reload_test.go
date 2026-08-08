package sybra

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/routing"
	"github.com/Automaat/sybra/internal/workflow"
	"gopkg.in/yaml.v3"
)

// setupConfigSvc creates a ConfigService wired to a temp SYBRA_HOME.
// Returns the service and the path to config.yaml for mutation.
func setupConfigSvc(t *testing.T) (svc *ConfigService, cfgPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

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
	rawSeed, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read seed config: %v", err)
	}
	if err := os.WriteFile(config.LastKnownGoodConfigPath(), rawSeed, 0o600); err != nil {
		t.Fatalf("write last-known-good seed: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	emit := func(string, any) {}
	logger := slog.New(slog.DiscardHandler)
	logDir := filepath.Join(home, "logs")
	mgr := newTestAgentManager(t, context.Background(), emit, logger, logDir, agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{
			MaxConcurrent:   cfg.Agent.MaxConcurrent,
			DefaultProvider: cfg.Agent.Provider,
		},
	})
	mgr.SetGuardrails(agent.Guardrails{
		MaxCostUSD:              cfg.Agent.MaxCostUSD,
		MaxTurns:                cfg.Agent.MaxTurns,
		MaxCheckpoints:          cfg.MaxCheckpoints(),
		CheckpointOnTurnCeiling: cfg.CheckpointOnTurnCeilingEnabled(),
	})

	notifier := notification.New(emit)

	svc = &ConfigService{
		cfg:       cfg,
		persisted: cloneConfig(cfg),
		logLevel:  logLevel,
		notifier:  notifier,
		agents:    mgr,
		logger:    logger,
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

func TestCloneConfigPreservesABTestingWeightVersion(t *testing.T) {
	builtinVersion := 6
	weightsVersion := 42
	src := &config.Config{
		ABTesting: abtest.Config{
			BuiltinVersion: &builtinVersion,
			WeightsVersion: &weightsVersion,
			Experiments: []abtest.Experiment{{
				ID: "exp",
				Variants: []abtest.Variant{{
					ID:     "v1",
					Weight: 1,
				}},
			}},
		},
	}

	got := cloneConfig(src)
	if got.ABTesting.WeightsVersion == nil || *got.ABTesting.WeightsVersion != weightsVersion {
		t.Fatalf("WeightsVersion = %v, want %d", got.ABTesting.WeightsVersion, weightsVersion)
	}
	if got.ABTesting.BuiltinVersion == nil || *got.ABTesting.BuiltinVersion != builtinVersion {
		t.Fatalf("BuiltinVersion = %v, want %d", got.ABTesting.BuiltinVersion, builtinVersion)
	}

	weightsVersion = 99
	builtinVersion = 99
	if *got.ABTesting.WeightsVersion != 42 {
		t.Fatalf("WeightsVersion shares source pointer, got %d", *got.ABTesting.WeightsVersion)
	}
	if *got.ABTesting.BuiltinVersion != 6 {
		t.Fatalf("BuiltinVersion shares source pointer, got %d", *got.ABTesting.BuiltinVersion)
	}
}

func TestReloadFromDisk_MaxConcurrent(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	// Rewrite config with higher concurrency
	next := *svc.cfg
	next.Agent.MaxConcurrent = 8
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(result.Applied, "agent") {
		t.Errorf("expected agent in applied, got %+v", result)
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
	next.Agent.MaxCheckpoints = 7
	disabled := false
	next.Agent.CheckpointOnTurnCeiling = &disabled
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(result.Applied, "agent") {
		t.Errorf("expected agent in applied, got %+v", result)
	}
	g := svc.agents.Guardrails()
	if g.MaxCostUSD != 20.0 {
		t.Errorf("Guardrails.MaxCostUSD = %v, want 20.0", g.MaxCostUSD)
	}
	if g.MaxTurns != 300 {
		t.Errorf("Guardrails.MaxTurns = %d, want 300", g.MaxTurns)
	}
	if g.MaxCheckpoints != 7 {
		t.Errorf("Guardrails.MaxCheckpoints = %d, want 7", g.MaxCheckpoints)
	}
	if g.CheckpointOnTurnCeiling {
		t.Error("Guardrails.CheckpointOnTurnCeiling = true, want false")
	}
}

func TestReloadFromDisk_WorkflowReviewGuardrails(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.workflowEngine = workflow.NewTestEngine(nil, nil, nil, slog.New(slog.DiscardHandler))
	svc.applyWorkflowGuardrails(*svc.cfg)

	next := *svc.cfg
	disabled := false
	next.Agent.ReviewUntilClean = &disabled
	next.Agent.MaxCheckpoints = 9
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(result.Applied, "agent") {
		t.Errorf("expected agent in applied, got %+v", result)
	}
	if svc.workflowEngine.ReviewUntilClean() {
		t.Error("workflow reviewLoopDisabled = false, want true")
	}
	if got := workflowIntField(t, svc.workflowEngine, "maxCheckpoints"); got != 9 {
		t.Errorf("workflow maxCheckpoints = %d, want 9", got)
	}
}

// TestApplyWorkflowGuardrails_WiresReviewRoundsPerHour locks in that the
// engine's rolling-hour review budget is wired from
// Agent.ReviewRoundsPerHourLimit. Agent.* is a hot-reload field
// (configApplyAgentRuntime), so ReloadFromDisk already exercises this path
// via the "agent" special case in applyHotChangesLocked; this test pins the
// wiring directly against applyWorkflowGuardrails so it fails on the exact
// function that dropped the value, rather than only on the reload plumbing
// around it.
func TestApplyWorkflowGuardrails_WiresReviewRoundsPerHour(t *testing.T) {
	svc, _ := setupConfigSvc(t)
	svc.workflowEngine = workflow.NewTestEngine(nil, nil, nil, slog.New(slog.DiscardHandler))

	cfg := *svc.cfg
	cfg.Agent.ReviewRoundsPerHour = 7
	svc.applyWorkflowGuardrails(cfg)

	if got := svc.workflowEngine.ReviewRoundsPerHour(); got != 7 {
		t.Errorf("workflow reviewRoundsPerHour = %d, want 7", got)
	}
}

func workflowIntField(t *testing.T, e *workflow.Engine, name string) int {
	t.Helper()
	v := reflect.ValueOf(e).Elem().FieldByName(name)
	if !v.IsValid() {
		t.Fatalf("workflow.Engine field %q not found", name)
	}
	return int(v.Int())
}

func TestReloadFromDisk_Provider(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	next := *svc.cfg
	next.Agent.Provider = "codex"
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(result.Applied, "agent") {
		t.Errorf("expected agent in applied, got %+v", result)
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

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(result.Applied, "logging.level") {
		t.Errorf("expected logging.level in applied, got %+v", result)
	}
	if svc.logLevel.Level() != slog.LevelDebug {
		t.Errorf("logLevel = %v, want Debug", svc.logLevel.Level())
	}
}

func TestReloadFromDisk_InvalidYAML(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	before, err := os.ReadFile(config.LastKnownGoodConfigPath())
	if err != nil {
		t.Fatalf("read last-known-good: %v", err)
	}

	if err := os.WriteFile(cfgPath, []byte(":::invalid yaml{{{\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origMax := svc.cfg.Agent.MaxConcurrent
	result, err := svc.ReloadFromDisk()
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if result.Recovery == nil || !result.Recovery.RestoredLastKnownGood {
		t.Fatalf("expected last-known-good recovery on invalid YAML, got %+v", result.Recovery)
	}
	if svc.cfg.Agent.MaxConcurrent != origMax {
		t.Errorf("cfg mutated on error: got %d, want %d", svc.cfg.Agent.MaxConcurrent, origMax)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("config.yaml not restored after invalid YAML\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestReloadFromDisk_ValidationError(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	before, err := os.ReadFile(config.LastKnownGoodConfigPath())
	if err != nil {
		t.Fatalf("read last-known-good: %v", err)
	}

	next := *svc.cfg
	next.Logging.Level = "verbose" // invalid
	writeConfigYAML(t, cfgPath, &next)

	origLevel := svc.cfg.Logging.Level
	result, err := svc.ReloadFromDisk()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if result.Recovery == nil || !result.Recovery.RestoredLastKnownGood {
		t.Fatalf("expected last-known-good recovery on validation error, got %+v", result.Recovery)
	}
	if svc.cfg.Logging.Level != origLevel {
		t.Errorf("cfg.Logging.Level mutated on validation error")
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("config.yaml not restored after validation error\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestReloadFromDisk_UnknownLegacyKeyRestoresLastKnownGood(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	before, err := os.ReadFile(config.LastKnownGoodConfigPath())
	if err != nil {
		t.Fatalf("read last-known-good: %v", err)
	}
	if err := os.WriteFile(cfgPath, append(before, []byte("\ntodoist:\n  enabled: true\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := svc.ReloadFromDisk()
	if err == nil {
		t.Fatal("expected unknown key error, got nil")
	}
	if !strings.Contains(err.Error(), "todoist") {
		t.Fatalf("error = %v, want todoist unknown-key context", err)
	}
	if result.Recovery == nil || !result.Recovery.RestoredLastKnownGood {
		t.Fatalf("expected last-known-good recovery on unknown key, got %+v", result.Recovery)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("config.yaml not restored after unknown legacy key\nbefore:\n%s\nafter:\n%s", before, after)
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
	records := make([]slog.Record, 0)
	handler := &recordHandler{records: &records}
	logger := slog.New(handler)

	emit := func(string, any) {}
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	logDir := filepath.Join(home, "logs")
	mgr := newTestAgentManager(t, context.Background(), emit, logger, logDir, agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{
			MaxConcurrent:   cfg.Agent.MaxConcurrent,
			DefaultProvider: cfg.Agent.Provider,
		},
	})
	mgr.SetGuardrails(agent.Guardrails{MaxCostUSD: cfg.Agent.MaxCostUSD, MaxTurns: cfg.Agent.MaxTurns})
	notifier := notification.New(emit)

	svc := &ConfigService{
		cfg:       cfg,
		persisted: cloneConfig(cfg),
		logLevel:  logLevel,
		notifier:  notifier,
		agents:    mgr,
		logger:    logger,
	}

	// Change a restart-required field
	next := *cfg
	next.Providers.HealthCheck.IntervalSeconds = 600
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("expected no applied keys, got %+v", result)
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

	// Restart-required changes stay pending, not active.
	if svc.cfg.Providers.HealthCheck.IntervalSeconds == 600 {
		t.Error("active cfg unexpectedly published restart-required provider health change")
	}
	if got := svc.GetSettings().Providers.HealthCheck.IntervalSeconds; got != 600 {
		t.Errorf("persisted settings provider health = %d, want 600", got)
	}

	// Second reload with same content: no new warnings
	prevCount := len(records)
	result2, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("second ReloadFromDisk: %v", err)
	}
	if len(result2.Applied) != 0 || len(result2.RestartRequired) != 0 {
		t.Errorf("second reload: expected no further changes, got %+v", result2)
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

func TestReloadFromDisk_BrowserRestartRequiredWarned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	seed := config.DefaultConfig()
	cfgPath := filepath.Join(home, "config.yaml")
	writeConfigYAML(t, cfgPath, seed)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	records := make([]slog.Record, 0)
	handler := &recordHandler{records: &records}
	logger := slog.New(handler)

	emit := func(string, any) {}
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	logDir := filepath.Join(home, "logs")
	mgr := newTestAgentManager(t, context.Background(), emit, logger, logDir, agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{
			MaxConcurrent:   cfg.Agent.MaxConcurrent,
			DefaultProvider: cfg.Agent.Provider,
		},
	})
	mgr.SetGuardrails(agent.Guardrails{MaxCostUSD: cfg.Agent.MaxCostUSD, MaxTurns: cfg.Agent.MaxTurns})
	notifier := notification.New(emit)

	svc := &ConfigService{
		cfg:       cfg,
		persisted: cloneConfig(cfg),
		logLevel:  logLevel,
		notifier:  notifier,
		agents:    mgr,
		logger:    logger,
	}

	next := *cfg
	enabled := true
	next.Browser.InApp = &enabled
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("expected no applied keys, got %+v", result)
	}

	found := false
	for _, r := range records {
		if r.Message == "config.reload.restart_required" {
			var field string
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "field" {
					field = a.Value.String()
				}
				return true
			})
			if field == "browser" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected browser restart warning, got none")
	}
	if svc.cfg.InAppBrowserEnabled() {
		t.Error("active cfg unexpectedly published restart-required browser change")
	}
	if got := svc.GetSettings().Browser.InApp; got == nil || !*got {
		t.Error("persisted browser setting not retained as pending")
	}
}

func TestReloadFromDisk_ServerRestartRequiredWarnedAndSynced(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.cfg.Server.AuthToken = "old-token"
	writeConfigYAML(t, cfgPath, svc.cfg)

	next := *svc.cfg
	next.Server.AuthToken = "new-token"
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("expected no applied keys, got %+v", result)
	}
	if svc.cfg.Server.AuthToken == "new-token" {
		t.Fatalf("active server auth token unexpectedly updated without restart")
	}
	if svc.persisted == nil || svc.persisted.Server.AuthToken != "new-token" {
		t.Fatalf("persisted server auth token = %q, want new-token", svc.persisted.Server.AuthToken)
	}
}

func TestReloadFromDisk_RefreshesLimitPolicyForProviderChanges(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	limitStore, err := limits.NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	svc.limits = limitStore
	svc.policy = func() limits.Policy {
		p := limits.DefaultPolicy()
		p.Enabled = svc.cfg.Providers.Limits.Enabled
		p.ProviderEnabled = map[string]bool{
			limits.ProviderClaude:  svc.cfg.Providers.Claude.Enabled,
			limits.ProviderCodex:   svc.cfg.Providers.Codex.Enabled,
			limits.ProviderCopilot: svc.cfg.Providers.Copilot.Enabled,
		}
		return p
	}
	if err := svc.refreshAgentRuntimeConfig(*svc.cfg); err != nil {
		t.Fatal(err)
	}
	if !svc.agents.LimitPolicy().ProviderEnabled[limits.ProviderCodex] {
		t.Fatal("initial manager limit policy did not enable codex")
	}

	next := *svc.cfg
	next.Providers.Codex.Enabled = false
	writeConfigYAML(t, cfgPath, &next)

	if _, err := svc.ReloadFromDisk(); err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if svc.agents.LimitPolicy().ProviderEnabled[limits.ProviderCodex] {
		t.Fatal("manager limit policy stayed stale after provider reload")
	}
}

func TestReloadFromDisk_RefreshesProviderHealthRuntimeFlags(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	svc.providerHealth = provider.New(provider.Config{
		ClaudeEnabled:   svc.cfg.Providers.Claude.Enabled,
		CodexEnabled:    svc.cfg.Providers.Codex.Enabled,
		CopilotEnabled:  svc.cfg.Providers.Copilot.Enabled,
		OpenCodeEnabled: svc.cfg.Providers.OpenCode.Enabled,
		AutoFailover:    svc.cfg.Providers.AutoFailover,
	}, nil, slog.New(slog.DiscardHandler))

	next := *svc.cfg
	next.Providers.AutoFailover = false
	next.Providers.Codex.Enabled = false
	writeConfigYAML(t, cfgPath, &next)

	if _, err := svc.ReloadFromDisk(); err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if svc.providerHealth.AutoFailover() {
		t.Fatal("provider health auto-failover flag stayed stale after provider reload")
	}
	if got := svc.providerHealth.Snapshot()["codex"]; got.Reason != "disabled" {
		t.Fatalf("codex health reason = %q, want disabled", got.Reason)
	}
}

// TestReloadFromDisk_EvidenceStaysPendingUntilRestart is the regression for the
// silent-override bug: agent.evidence is restart-policy because the workflow
// engine caches it once at init. A hot reload of only that flag must NOT copy
// the new value into the live cfg (that would desync the engine from the store),
// only surface it as pending until restart.
func TestReloadFromDisk_EvidenceStaysPendingUntilRestart(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	origEvidence := svc.cfg.Agent.Evidence.Enabled

	next := *svc.cfg
	next.Agent.Evidence.Enabled = !origEvidence
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(result.RestartRequired, "agent.evidence") {
		t.Errorf("expected agent.evidence in restartRequired, got %+v", result)
	}
	if slices.Contains(result.Applied, "agent") {
		t.Errorf("agent must not be applied for an evidence-only change, got %+v", result)
	}
	if svc.cfg.Agent.Evidence.Enabled != origEvidence {
		t.Errorf("active cfg evidence flag updated without restart: got %v, want %v",
			svc.cfg.Agent.Evidence.Enabled, origEvidence)
	}
	if svc.persisted == nil || svc.persisted.Agent.Evidence.Enabled != !origEvidence {
		t.Errorf("persisted evidence flag = %v, want pending %v",
			svc.persisted.Agent.Evidence.Enabled, !origEvidence)
	}
}

func TestReloadFromDisk_NoFeedbackLoop(t *testing.T) {
	// UpdateSettings saves to disk; watcher fires ReloadFromDisk; diff should
	// be empty since disk now matches in-memory cfg.
	svc, _ := setupConfigSvc(t)

	// UpdateSettings mutates cfg and saves. Build from the live round-tripped
	// payload so newly added config sections participate in the test instead of
	// silently regressing it to a partial overlay.
	settings := configToSettings(svc.cfg)
	settings.Logging = LoggingSettings{
		Level:     "warn",
		MaxSizeMB: svc.cfg.Logging.MaxSizeMB,
		MaxFiles:  svc.cfg.Logging.MaxFiles,
	}
	settings.Agent.MaxConcurrent = 3 // ensure valid
	if _, err := svc.UpdateSettings(settings); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// Simulate watcher-triggered reload — disk now matches in-memory
	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if len(result.Applied) != 0 || len(result.RestartRequired) != 0 {
		t.Errorf("feedback loop: expected empty diff after UpdateSettings+Reload, got %+v", result)
	}
}

func TestReloadFromDisk_ResultListsAppliedRestartAndUnchanged(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	next := *svc.cfg
	next.Logging.Level = "debug"
	enabled := true
	next.Browser.InApp = &enabled
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(result.Applied, "logging.level") {
		t.Fatalf("expected logging.level in applied, got %+v", result)
	}
	if !slices.Contains(result.RestartRequired, "browser") {
		t.Fatalf("expected browser in restartRequired, got %+v", result)
	}
	if !slices.Contains(result.Unchanged, "audit") {
		t.Fatalf("expected unchanged paths to include audit, got %+v", result)
	}
}

func TestReloadFromDisk_ReadersSeeWholePersistedSnapshots(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	done := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				got := svc.GetSettings()
				inApp := got.Browser.InApp != nil && *got.Browser.InApp
				key := got.Agent.Provider + "|" + got.Logging.Level
				switch {
				case key == "claude|info" && !inApp:
				case key == "codex|debug" && inApp:
				default:
					errCh <- errors.New("observed mixed settings snapshot during reload")
					return
				}
			}
		}
	}()

	next := *svc.cfg
	next.Agent.Provider = "codex"
	next.Logging.Level = "debug"
	enabled := true
	next.Browser.InApp = &enabled
	writeConfigYAML(t, cfgPath, &next)
	if _, err := svc.ReloadFromDisk(); err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	close(done)

	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

func TestMutateLocked_PublishesImmutableAppSnapshot(t *testing.T) {
	initial := config.DefaultConfig()
	app := NewApp(slog.New(slog.DiscardHandler), new(slog.LevelVar), initial)
	svc := &ConfigService{
		cfg:       initial,
		persisted: cloneConfig(initial),
		publishConfig: func(next *config.Config) {
			app.activeCfg.Store(next)
		},
	}

	var readers sync.WaitGroup
	done := make(chan struct{})
	readers.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				_ = app.currentConfig().Cluster.Followers
			}
		}
	})

	next := cloneConfig(initial)
	next.Logging.Level = "debug"
	svc.mu.Lock()
	_, _, err := svc.mutateLocked(next, nil)
	svc.mu.Unlock()
	close(done)
	readers.Wait()
	if err != nil {
		t.Fatalf("mutateLocked: %v", err)
	}
	if got := app.currentConfig(); got != svc.cfg {
		t.Fatal("App did not receive the published config snapshot")
	}
	if app.cfg == svc.cfg {
		t.Fatal("hot reload mutated the construction-time config snapshot")
	}
}

func TestSaveRawConfig_RestoresLastKnownGoodOnHotApplyFailure(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)
	writeConfigYAML(t, cfgPath, svc.cfg)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := svc.GetRawConfig()
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(raw, "provider: claude", "provider: codex", 1)
	svc.applyRuntime = func(config.Config) error { return errors.New("boom") }

	err = svc.SaveRawConfig(edited)
	if err == nil {
		t.Fatal("expected hot apply failure, got nil")
	}
	var mutErr *configMutationError
	if !errors.As(err, &mutErr) || mutErr.result.Recovery == nil || !mutErr.result.Recovery.RestoredLastKnownGood {
		t.Fatalf("expected recovery result on hot apply failure, got %v", err)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("config.yaml not restored after hot apply failure\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if svc.cfg.Agent.Provider != "claude" {
		t.Fatalf("active cfg mutated after failed hot apply: provider=%s", svc.cfg.Agent.Provider)
	}
}

// TestReloadFromDisk_ABTestingPreservesRoutingOverlay is the regression for a
// base ab_testing hot-edit silently dropping the live routing overlay: the
// reload replaces cfg.ABTesting with the plain operator-saved base, so without
// an immediate overlay re-merge every selection site would dispatch on
// unweighted base until the next routing tick (hours). The fix re-invokes the
// routing service right after the base swap.
func TestReloadFromDisk_ABTestingPreservesRoutingOverlay(t *testing.T) {
	svc, cfgPath := setupConfigSvc(t)

	base := abtest.Config{
		MinSamplesPerVariant: 20,
		Experiments: []abtest.Experiment{{
			ID: "exp",
			// Model is required by variant validation; config.Load drops
			// experiments that would fail selection-time validation, so an
			// underspecified fixture would not survive the reload.
			Variants: []abtest.Variant{
				{ID: "v1", Provider: "claude", Model: "opus", Weight: 1},
				{ID: "v2", Provider: "codex", Model: "gpt-5.5", Weight: 1},
			},
		}},
	}
	svc.cfg.ABTesting = base
	svc.persisted.ABTesting = base

	store, err := routing.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("routing.NewStore: %v", err)
	}
	if err := store.Save(routing.Overlay{
		Version: 3,
		Experiments: []routing.OverlayExperiment{{
			ExperimentID: "exp",
			Variants: []routing.OverlayVariant{
				{VariantID: "v1", Weight: 8},
				{VariantID: "v2", Weight: 2},
			},
		}},
	}); err != nil {
		t.Fatalf("store.Save: %v", err)
	}
	routingSvc := routing.NewService(routing.Deps{
		Cfg:   config.RoutingConfig{Enabled: true},
		Base:  func() abtest.Config { return svc.cfg.ABTesting },
		Store: store,
		Apply: func(c abtest.Config) error { svc.cfg.ABTesting = c; return nil },
	})
	svc.reapplyRouting = routingSvc.ApplyPersistedOverlay

	// Operator edits the base A/B suite (min samples) on disk.
	next := *svc.cfg
	next.ABTesting.MinSamplesPerVariant = 30
	writeConfigYAML(t, cfgPath, &next)

	result, err := svc.ReloadFromDisk()
	if err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}
	if !slices.Contains(result.Applied, "ab_testing") {
		t.Fatalf("expected ab_testing in applied, got %+v", result.Applied)
	}

	got := svc.cfg.ABTesting
	if got.MinSamplesPerVariant != 30 {
		t.Errorf("MinSamplesPerVariant = %d, want 30 (new base)", got.MinSamplesPerVariant)
	}
	if got.WeightsVersion == nil || *got.WeightsVersion != 3 {
		t.Fatalf("WeightsVersion = %v, want 3 (overlay re-merged)", got.WeightsVersion)
	}
	// Locate the operator's experiment by ID — config.Load reconciles builtin
	// experiments into ab_testing, so it is not the only entry.
	var exp *abtest.Experiment
	for i := range got.Experiments {
		if got.Experiments[i].ID == "exp" {
			exp = &got.Experiments[i]
			break
		}
	}
	if exp == nil || len(exp.Variants) != 2 {
		t.Fatalf("exp experiment missing/malformed after reload: %+v", got.Experiments)
	}
	if w := exp.Variants[0].Weight; w != 8 {
		t.Errorf("v1 weight = %d, want 8 (overlay preserved, not base 1)", w)
	}
	if w := exp.Variants[1].Weight; w != 2 {
		t.Errorf("v2 weight = %d, want 2 (overlay preserved, not base 1)", w)
	}
}

// recordHandler captures log records for assertion in tests.
type recordHandler struct {
	mu      sync.Mutex
	records *[]slog.Record
	slog.Handler
}

func (h *recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r)
	return nil
}
func (h *recordHandler) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(*h.records)
}
func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }
