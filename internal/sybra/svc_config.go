package sybra

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/workflow"
	"gopkg.in/yaml.v3"
)

// ConfigService exposes settings read/write as Wails-bound methods.
type ConfigService struct {
	mu             sync.RWMutex
	cfg            *config.Config
	logLevel       *slog.LevelVar
	notifier       *notification.Emitter
	agents         *agent.Manager
	limits         *limits.Store
	workflowEngine *workflow.Engine
	logger         *slog.Logger
	policy         func() limits.Policy
}

// GetSettings returns the current app settings for the config UI.
func (s *ConfigService) GetSettings() AppSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	return AppSettings{
		Agent:        c.Agent,
		Notification: c.Notification,
		Orchestrator: c.Orchestrator,
		Logging: LoggingSettings{
			Level:     c.Logging.Level,
			MaxSizeMB: c.Logging.MaxSizeMB,
			MaxFiles:  c.Logging.MaxFiles,
		},
		Audit:        c.Audit,
		Renovate:     c.Renovate,
		Providers:    c.Providers,
		GitHub:       c.GitHub,
		Monitor:      c.Monitor,
		SelfMonitor:  c.SelfMonitor,
		Triage:       c.Triage,
		Umbrella:     c.Umbrella,
		Testing:      c.Testing,
		Experience:   c.Experience,
		Metrics:      c.Metrics,
		Browser:      c.Browser,
		ProjectTypes: c.ProjectTypes,
		Directories:  c.Directories(),
	}
}

// GetDefaultSettings returns the settings an empty config file resolves to. The
// UI diffs live values against these to flag "modified from default" fields and
// to power per-field reset-to-default, without hardcoding defaults in
// TypeScript.
func (s *ConfigService) GetDefaultSettings() AppSettings {
	return configToSettings(config.DefaultConfig())
}

// GetRawConfig returns the raw config.yaml text for the Advanced (YAML) editor.
// Unlike GetSettings this is NOT redacted — it is the user's own local file,
// surfaced behind an explicit Advanced disclosure with a secrets warning.
func (s *ConfigService) GetRawConfig() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return config.ReadRawConfig()
}

// SaveRawConfig validates raw YAML, atomically writes it (preserving the user's
// formatting and comments), then hot-reloads. Invalid YAML or a value that
// fails validation is rejected without touching disk.
func (s *ConfigService) SaveRawConfig(raw string) error {
	saveRaw := []byte(raw)
	var err error
	s.mu.RLock()
	preserveServerToken := serverAuthTokenForRawSave(s.cfg)
	s.mu.RUnlock()
	if preserveServerToken != "" {
		saveRaw, err = ensureServerAuthTokenInRawConfig(saveRaw, preserveServerToken)
		if err != nil {
			return validationError(fmt.Sprintf("invalid config: %s", err))
		}
	}
	fileCfg, err := config.ParseFileConfig(saveRaw)
	if err != nil {
		return validationError(fmt.Sprintf("invalid config: %s", err))
	}
	if _, err := config.ResolveFromCurrentEnvironment(fileCfg, config.ResolveOptions{}); err != nil {
		return validationError(err.Error())
	}
	if err := config.WriteRawConfig(saveRaw); err != nil {
		return err
	}
	_, err = s.ReloadFromDisk()
	return err
}

func serverAuthTokenForRawSave(cfg *config.Config) string {
	if cfg == nil || strings.TrimSpace(os.Getenv("SYBRA_AUTH_TOKEN")) != "" {
		return ""
	}
	return strings.TrimSpace(cfg.Server.AuthToken)
}

func ensureServerAuthTokenInRawConfig(raw []byte, token string) ([]byte, error) {
	fileCfg, err := config.ParseFileConfig(raw)
	if err != nil {
		return nil, err
	}
	if fileCfg.Has("server", "auth_token") {
		return raw, nil
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	keyNode, valueNode, ok := yamlTopLevelField(&root, "server")
	if !ok {
		return appendServerBlock(raw, token), nil
	}
	if valueNode.Kind != yaml.MappingNode {
		return rewriteRawConfigServerAuthToken(&root, keyNode, valueNode, token)
	}
	return insertServerAuthTokenLine(raw, keyNode, token)
}

func yamlTopLevelField(root *yaml.Node, key string) (keyNode, valueNode *yaml.Node, ok bool) {
	node := root
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil, nil, false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i], node.Content[i+1], true
		}
	}
	return nil, nil, false
}

func appendServerBlock(raw []byte, token string) []byte {
	trimmed := strings.TrimRight(string(raw), "\n")
	if trimmed != "" {
		trimmed += "\n"
	}
	return []byte(trimmed + "server:\n  auth_token: " + yamlScalar(token) + "\n")
}

func rewriteRawConfigServerAuthToken(root, keyNode, valueNode *yaml.Node, token string) ([]byte, error) {
	if valueNode.Kind == 0 {
		valueNode.Kind = yaml.MappingNode
		valueNode.Tag = "!!map"
		valueNode.Value = ""
		valueNode.Style = 0
	}
	valueNode.Kind = yaml.MappingNode
	valueNode.Tag = "!!map"
	valueNode.Content = append(valueNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "auth_token"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: token},
	)
	if keyNode != nil && keyNode.HeadComment != "" && valueNode.HeadComment == "" {
		valueNode.HeadComment = keyNode.HeadComment
	}
	var out strings.Builder
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

func insertServerAuthTokenLine(raw []byte, keyNode *yaml.Node, token string) ([]byte, error) {
	lines := strings.Split(string(raw), "\n")
	serverIndent := keyNode.Column - 1
	insertAt := len(lines)
	for i := keyNode.Line; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= serverIndent {
			insertAt = i
			break
		}
	}
	entry := strings.Repeat(" ", serverIndent+2) + "auth_token: " + yamlScalar(token)
	lines = append(lines, "")
	copy(lines[insertAt+1:], lines[insertAt:])
	lines[insertAt] = entry
	return []byte(strings.Join(lines, "\n")), nil
}

func yamlScalar(s string) string {
	data, err := yaml.Marshal(s)
	if err != nil {
		return s
	}
	return strings.TrimSpace(string(data))
}

// UpdateSettings validates, persists, and hot-reloads the provided settings.
func (s *ConfigService) UpdateSettings(settings AppSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.validateSettings(settings); err != nil {
		return err
	}
	if err := s.applyFromConfig(settingsToConfig(s.cfg, settings)); err != nil {
		return err
	}
	return s.cfg.Save()
}

// validateSettings checks all editable fields for validity.
func (s *ConfigService) validateSettings(settings AppSettings) error {
	next := settingsToConfig(s.cfg, settings)
	if err := config.ValidateResolvedConfig(&next); err != nil {
		return validationError(err.Error())
	}
	return nil
}

// applyFromConfig assigns all hot-reloadable fields from next into s.cfg and
// pushes the manager settings that are intentionally live. s.mu must be held by the caller.
// This never writes to disk — callers that need persistence must call s.cfg.Save().
func (s *ConfigService) applyFromConfig(next config.Config) error {
	s.cfg.Agent = next.Agent
	s.cfg.Notification = next.Notification
	s.cfg.Orchestrator = next.Orchestrator
	s.cfg.Logging.Level = next.Logging.Level
	s.cfg.Logging.MaxSizeMB = next.Logging.MaxSizeMB
	s.cfg.Logging.MaxFiles = next.Logging.MaxFiles
	s.cfg.Audit = next.Audit
	// In-place field assignment: the renovate coordinator holds &s.cfg.Renovate.
	s.cfg.Renovate.Enabled = next.Renovate.Enabled
	s.cfg.Renovate.Author = next.Renovate.Author
	s.cfg.Providers = next.Providers
	s.cfg.GitHub = next.GitHub
	s.cfg.Triage = next.Triage
	s.cfg.Monitor = next.Monitor
	s.cfg.SelfMonitor = next.SelfMonitor
	s.cfg.Umbrella = next.Umbrella
	s.cfg.Testing = next.Testing
	s.cfg.Experience = next.Experience
	s.cfg.Browser = next.Browser
	s.cfg.ABTesting = next.ABTesting
	s.cfg.Metrics = next.Metrics
	s.cfg.ProjectTypes = next.ProjectTypes
	s.notifier.SetDesktop(next.Notification.Desktop)
	if err := s.refreshAgentRuntimeConfig(next); err != nil {
		return err
	}
	if s.agents != nil {
		s.agents.SetGuardrails(agent.Guardrails{
			MaxCostUSD:              next.Agent.MaxCostUSD,
			MaxTurns:                next.Agent.MaxTurns,
			MaxCheckpoints:          next.MaxCheckpoints(),
			TurnCostFraction:        next.Agent.TurnCostFraction,
			TurnMultiplier:          next.Agent.TurnMultiplier,
			CheckpointOnTurnCeiling: next.CheckpointOnTurnCeilingEnabled(),
		})
	}
	if s.workflowEngine != nil {
		s.workflowEngine.SetMaxCheckpoints(next.MaxCheckpoints())
	}
	if s.logLevel != nil {
		s.logLevel.Set(s.cfg.Logging.SlogLevel())
	}
	return nil
}

func (s *ConfigService) refreshAgentRuntimeConfig(next config.Config) error {
	if s.agents == nil {
		return nil
	}
	return s.agents.ReplaceRuntimeConfig(s.managerRuntimeConfig(next))
}

func (s *ConfigService) managerRuntimeConfig(cfg config.Config) agent.ManagerRuntimeConfig {
	policy := limits.DefaultPolicy()
	if s.policy != nil {
		policy = s.policy()
	}
	return agent.ManagerRuntimeConfig{
		MaxConcurrent:          cfg.Agent.MaxConcurrent,
		DefaultProvider:        cfg.Agent.Provider,
		DefaultModel:           cfg.Agent.Model,
		BashTimeoutMs:          cfg.BashTimeoutMs(),
		RetryWatchdog:          cfg.RetryWatchdog(),
		FallbackModel:          cfg.Agent.FallbackModel,
		LimitGate:              agent.LimitGateOrNil(s.limits),
		LimitPolicy:            policy,
		MaxInFlightPerProvider: cfg.Providers.Limits.MaxInFlightPerProvider,
		DispatchJitterMs:       cfg.Agent.DispatchJitterMs,
		HeadlessSteerable:      cfg.DefaultHeadlessSteerable(),
		SandboxMode:            cfg.DefaultSandboxMode(),
		PlaywrightMCPEnabled:   cfg.PlaywrightMCPEnabled(),
		PlaywrightMCPExtraArgs: cfg.PlaywrightMCPExtraArgs(),
		K8sJobsEnabled:         cfg.Agent.K8sJobs.Enabled,
		K8sJobs:                k8sJobRunnerConfigFromConfig(cfg.Agent.K8sJobs),
	}
}

// settingsToConfig converts AppSettings into a config.Config overlay, filling
// fields not present in AppSettings from the existing cfg.
func settingsToConfig(existing *config.Config, settings AppSettings) config.Config {
	next := *existing
	next.Agent = settings.Agent
	next.Notification = settings.Notification
	next.Orchestrator = settings.Orchestrator
	next.Logging.Level = settings.Logging.Level
	next.Logging.MaxSizeMB = settings.Logging.MaxSizeMB
	next.Logging.MaxFiles = settings.Logging.MaxFiles
	next.Audit = settings.Audit
	next.Renovate = settings.Renovate
	next.Providers = settings.Providers
	next.GitHub = settings.GitHub
	next.Monitor = settings.Monitor
	next.SelfMonitor = settings.SelfMonitor
	next.Triage = settings.Triage
	next.Umbrella = settings.Umbrella
	next.Testing = settings.Testing
	next.Experience = settings.Experience
	next.Metrics = settings.Metrics
	next.Browser = settings.Browser
	next.ProjectTypes = settings.ProjectTypes
	return next
}
