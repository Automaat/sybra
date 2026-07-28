package sybra

import (
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/workflow"
	"gopkg.in/yaml.v3"
)

// ConfigService exposes settings read/write as Wails-bound methods.
type ConfigService struct {
	mu             sync.RWMutex
	cfg            *config.Config
	persisted      *config.Config
	logLevel       *slog.LevelVar
	notifier       *notification.Emitter
	agents         *agent.Manager
	limits         *limits.Store
	providerHealth *provider.Checker
	workflowEngine *workflow.Engine
	logger         *slog.Logger
	policy         func() limits.Policy
	applyRuntime   func(config.Config) error
	// reapplyRouting re-merges the persisted routing overlay on top of the
	// freshly hot-reloaded base A/B config and fans it back out to every
	// selection site. Called after an ab_testing hot change so a base edit
	// does not silently drop the live overlay until the next routing tick.
	reapplyRouting func()
	// applyABTestingBase publishes an operator-authored ab_testing hot reload
	// to direct dispatch sites before any persisted routing overlay is remerged.
	applyABTestingBase func(abtest.Config)
}

// GetSettings returns the current app settings for the config UI.
func (s *ConfigService) GetSettings() AppSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	if s.persisted != nil {
		c = s.persisted
	}
	return AppSettings{
		Agent:        c.Agent,
		Notification: c.Notification,
		Orchestrator: c.Orchestrator,
		Logging: LoggingSettings{
			Level:     c.Logging.Level,
			MaxSizeMB: c.Logging.MaxSizeMB,
			MaxFiles:  c.Logging.MaxFiles,
		},
		Attachments: c.Attachments,
		Audit:       c.Audit,
		Renovate:    c.Renovate,
		Providers:   c.Providers,
		ProviderRouting: ProviderRoutingSettings{
			ABTestingEnabled:              c.ABTesting.Enabled,
			ABTestingMinSamplesPerVariant: c.ABTesting.MinSamplesPerVariant,
			Summary:                       config.BuildRoutingSummary(c),
		},
		GitHub:       githubSettingsWithoutSecrets(c.GitHub),
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

func (s *ConfigService) GetPathExplanations() ([]ConfigPathExplanation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return LoadConfigPathExplanations(s.cfg, s.persisted)
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
	base := s.cfg
	if s.persisted != nil {
		base = s.persisted
	}
	s.mu.RUnlock()
	preserveServerToken, err := serverAuthTokenForRawSave(base)
	if err != nil {
		return validationError(fmt.Sprintf("invalid config: %s", err))
	}
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
	resolved, err := config.ResolveFromCurrentEnvironment(fileCfg, config.ResolveOptions{})
	if err != nil {
		return validationError(err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.mutateLocked(resolved.Config, func() error {
		return config.WriteRawConfig(saveRaw)
	})
	return err
}

// serverAuthTokenForRawSave returns the server auth token a raw save must
// preserve, but only when the operator's own config.yaml already declares
// server.auth_token explicitly. cfg.Server.AuthToken is also resolved from
// the SYBRA_AUTH_TOKEN env var or the generated server_auth_token file (see
// ensureServerAuthToken) — neither of those sources belongs in config.yaml,
// so an edit that omits the key must never materialize it there.
func serverAuthTokenForRawSave(cfg *config.Config) (string, error) {
	if cfg == nil || strings.TrimSpace(os.Getenv("SYBRA_AUTH_TOKEN")) != "" {
		return "", nil
	}
	existing, err := config.ReadRawConfig()
	if err != nil {
		return "", err
	}
	existingFileCfg, err := config.ParseFileConfig([]byte(existing))
	if err != nil {
		return "", err
	}
	if !existingFileCfg.Has("server", "auth_token") {
		return "", nil
	}
	return strings.TrimSpace(cfg.Server.AuthToken), nil
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
func (s *ConfigService) UpdateSettings(settings AppSettings) (ConfigMutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.validateSettings(settings); err != nil {
		return ConfigMutationResult{}, err
	}
	base := s.cfg
	if s.persisted != nil {
		base = s.persisted
	}
	next := settingsToConfig(base, settings)
	raw, err := config.ReadRawConfig()
	if err != nil {
		return ConfigMutationResult{}, err
	}
	patched, err := patchSettingsRawConfig([]byte(raw), base, &next)
	if err != nil {
		return ConfigMutationResult{}, err
	}
	return s.mutateLocked(&next, func() error {
		return config.WriteRawConfig(patched)
	})
}

// validateSettings checks all editable fields for validity.
func (s *ConfigService) validateSettings(settings AppSettings) error {
	base := s.cfg
	if s.persisted != nil {
		base = s.persisted
	}
	next := settingsToConfig(base, settings)
	if err := config.ValidateResolvedConfig(&next); err != nil {
		return validationError(err.Error())
	}
	return nil
}

func (s *ConfigService) mutateLocked(candidate *config.Config, persist func() error) (ConfigMutationResult, error) {
	current := cloneConfig(s.cfg)
	intent := cloneConfig(s.cfg)
	if s.persisted != nil {
		intent = cloneConfig(s.persisted)
	}
	result := diffConfig(*intent, *candidate)
	if len(result.Rejected) > 0 {
		return result, configMutationErrorf(result, "rejected immutable config paths: %s", strings.Join(result.Rejected, ", "))
	}
	nextActive := cloneConfig(current)
	// Copy the exact changed leaves, not the registry-entry paths: a hot ancestor
	// entry (e.g. "agent") can cover a restart-policy child (e.g. "agent.evidence")
	// whose change must stay pending until restart. appliedLeaves already excludes
	// those child leaves.
	for _, path := range result.appliedLeaves {
		copyConfigPath(nextActive, candidate, path)
	}
	if persist != nil {
		if err := persist(); err != nil {
			return result, err
		}
	}
	if err := s.applyHotChangesLocked(result, nextActive); err != nil {
		if persist != nil {
			if restoreErr := config.RestoreLastKnownGoodConfig(); restoreErr != nil {
				result.Recovery = &ConfigRecovery{
					Message: fmt.Sprintf("hot apply failed and last-known-good restore failed: %v", restoreErr),
				}
				return result, fmt.Errorf("%w; restore last-known-good: %w", err, restoreErr)
			}
			result.Recovery = &ConfigRecovery{
				RestoredLastKnownGood: true,
				Message:               "restored config.yaml from last-known-good after hot apply failure",
			}
		}
		return result, &configMutationError{result: result, cause: err}
	}
	*s.cfg = *nextActive
	s.persisted = cloneConfig(candidate)
	if slices.Contains(result.Applied, "ab_testing") && s.applyABTestingBase != nil {
		s.applyABTestingBase(nextActive.ABTesting)
	}
	// A base ab_testing hot change replaces live routing weights with the plain
	// operator-saved base. Re-merge the persisted overlay on top of that new
	// base now so every selection site stays weight-consistent instead of
	// drifting on unweighted base until the next routing tick.
	if s.reapplyRouting != nil && slices.Contains(result.Applied, "ab_testing") {
		s.reapplyRouting()
	}
	if s.logger != nil {
		for _, path := range result.RestartRequired {
			s.logger.Warn("config.reload.restart_required", "field", path)
		}
	}
	return result, nil
}

func copyConfigPath(dst, src *config.Config, path string) {
	dstField := fieldByYAMLPath(reflect.ValueOf(dst), path)
	srcField := fieldByYAMLPath(reflect.ValueOf(src), path)
	dstField.Set(srcField)
}

func (s *ConfigService) applyHotChangesLocked(result ConfigMutationResult, nextActive *config.Config) error {
	for _, group := range configApplyGroups(result.Applied) {
		switch group {
		case configApplyNone:
			continue
		case configApplyAgentRuntime:
			if err := s.refreshAgentRuntimeConfig(*nextActive); err != nil {
				return err
			}
		case configApplyGuardrails:
			s.applyAgentGuardrails(*nextActive)
		case configApplyNotification:
			if s.notifier != nil {
				s.notifier.SetDesktop(nextActive.Notification.Desktop)
			}
		case configApplyLogLevel:
			if s.logLevel != nil {
				s.logLevel.Set(nextActive.Logging.SlogLevel())
			}
		}
	}
	if slices.Contains(result.Applied, "agent") {
		s.applyAgentGuardrails(*nextActive)
	}
	if providerHealthRuntimeChanged(result.Applied) {
		s.applyProviderHealthRuntime(*nextActive)
	}
	return nil
}

func providerHealthRuntimeChanged(paths []string) bool {
	for _, path := range paths {
		switch path {
		case "providers.auto_failover", "providers.claude", "providers.codex", "providers.copilot", "providers.opencode":
			return true
		}
	}
	return false
}

func (s *ConfigService) applyProviderHealthRuntime(cfg config.Config) {
	if s.providerHealth == nil {
		return
	}
	s.providerHealth.SetAutoFailover(cfg.Providers.AutoFailover)
	s.providerHealth.SetProviderEnabled("claude", cfg.Providers.Claude.Enabled)
	s.providerHealth.SetProviderEnabled("codex", cfg.Providers.Codex.Enabled)
	s.providerHealth.SetProviderEnabled("copilot", cfg.Providers.Copilot.Enabled)
	s.providerHealth.SetProviderEnabled("opencode", cfg.Providers.OpenCode.Enabled)
}

func (s *ConfigService) applyAgentGuardrails(cfg config.Config) {
	if s.agents != nil {
		s.agents.SetGuardrails(agent.Guardrails{
			MaxCostUSD:              cfg.Agent.MaxCostUSD,
			MaxTurns:                cfg.Agent.MaxTurns,
			MaxCheckpoints:          cfg.MaxCheckpoints(),
			TurnCostFraction:        cfg.Agent.TurnCostFraction,
			TurnMultiplier:          cfg.Agent.TurnMultiplier,
			CheckpointOnTurnCeiling: cfg.CheckpointOnTurnCeilingEnabled(),
		})
	}
	s.applyWorkflowGuardrails(cfg)
}

func (s *ConfigService) applyWorkflowGuardrails(cfg config.Config) {
	if s.workflowEngine == nil {
		return
	}
	s.workflowEngine.SetMaxCheckpoints(cfg.MaxCheckpoints())
	s.workflowEngine.SetReviewUntilClean(cfg.ReviewUntilClean())
	s.workflowEngine.SetReviewRoundsPerHour(cfg.Agent.ReviewRoundsPerHourLimit())
}

func (s *ConfigService) refreshAgentRuntimeConfig(next config.Config) error {
	if s.applyRuntime != nil {
		return s.applyRuntime(next)
	}
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
	policy.Enabled = cfg.Providers.Limits.Enabled
	if policy.ProviderEnabled == nil {
		policy.ProviderEnabled = map[string]bool{}
	}
	policy.ProviderEnabled[limits.ProviderClaude] = cfg.Providers.Claude.Enabled
	policy.ProviderEnabled[limits.ProviderCodex] = cfg.Providers.Codex.Enabled
	policy.ProviderEnabled[limits.ProviderCopilot] = cfg.Providers.Copilot.Enabled
	policy.ProviderEnabled[limits.ProviderOpenCode] = cfg.Providers.OpenCode.Enabled
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
		ClassReservations:      agent.ParseClassReservations(cfg.Agent.ClassReservations),
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
	next.Attachments = settings.Attachments
	if next.Attachments.MaxSizeMB == 0 {
		next.Attachments.MaxSizeMB = existing.Attachments.MaxSizeMB
		if next.Attachments.MaxSizeMB == 0 {
			next.Attachments.MaxSizeMB = config.DefaultAttachmentMaxSizeMB
		}
	}
	next.Renovate = settings.Renovate
	next.Providers = settings.Providers
	if settings.ProviderRouting.ABTestingEnabled != nil {
		next.ABTesting.Enabled = settings.ProviderRouting.ABTestingEnabled
	}
	if settings.ProviderRouting.ABTestingMinSamplesPerVariant > 0 {
		next.ABTesting.MinSamplesPerVariant = settings.ProviderRouting.ABTestingMinSamplesPerVariant
	}
	next.GitHub = settings.GitHub
	if next.GitHub.Webhook.Secret == config.RedactedPlaceholder {
		next.GitHub.Webhook.Secret = existing.GitHub.Webhook.Secret
	}
	if next.GitHub.Webhook.TaskSecret == config.RedactedPlaceholder {
		next.GitHub.Webhook.TaskSecret = existing.GitHub.Webhook.TaskSecret
	}
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
