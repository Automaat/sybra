package sybra

import (
	"slices"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
)

// ReloadFromDisk re-reads ~/.sybra/config.yaml, validates it, diffs against
// the in-memory config, and applies hot-reloadable changes. Returns the list
// of hot-reloadable keys that changed. On any error the in-memory config is
// left unchanged. Never writes to disk.
func (s *ConfigService) ReloadFromDisk() (changedHot []string, err error) {
	next, err := config.Load()
	if err != nil {
		return nil, err
	}

	nextSettings := configToSettings(next)
	if err := s.validateSettings(nextSettings); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	hot, restart := diffConfig(*s.cfg, *next)

	// Always update s.cfg to match disk — including restart-required fields —
	// so subsequent reloads with the same content produce no diff and no
	// repeated restart warnings. renovateHandler holds &s.cfg.Renovate, so
	// assign its fields in-place rather than replacing the whole struct.
	s.cfg.Agent = next.Agent
	s.cfg.Notification = next.Notification
	s.cfg.Orchestrator = next.Orchestrator
	s.cfg.Logging.Level = next.Logging.Level
	s.cfg.Logging.MaxSizeMB = next.Logging.MaxSizeMB
	s.cfg.Logging.MaxFiles = next.Logging.MaxFiles
	s.cfg.Audit = next.Audit
	s.cfg.Todoist = next.Todoist
	s.cfg.Renovate.Enabled = next.Renovate.Enabled
	s.cfg.Renovate.Author = next.Renovate.Author
	s.cfg.Providers = next.Providers
	s.cfg.GitHub = next.GitHub
	s.cfg.Triage = next.Triage
	s.cfg.Monitor = next.Monitor
	s.cfg.SelfMonitor = next.SelfMonitor
	s.cfg.Metrics = next.Metrics
	s.cfg.ProjectTypes = next.ProjectTypes

	// Selectively call live setters only for fields that actually changed.
	// This avoids restarting Todoist or other services on every config write
	// when nothing relevant changed (e.g. UI saves non-Todoist settings).
	for _, k := range hot {
		switch k {
		case "notification.desktop":
			s.notifier.SetDesktop(next.Notification.Desktop)
		case "agent.max_concurrent":
			s.agents.SetMaxConcurrent(next.Agent.MaxConcurrent)
		case "agent.provider":
			s.agents.SetDefaultProvider(next.Agent.Provider)
		case "logging.level":
			if s.logLevel != nil {
				s.logLevel.Set(next.Logging.SlogLevel())
			}
		case "todoist":
			if s.reloadHook != nil {
				s.reloadHook()
			}
		}
	}
	// SetGuardrails once if either guardrail field changed.
	if slices.Contains(hot, "agent.max_cost_usd") || slices.Contains(hot, "agent.max_turns") {
		s.agents.SetGuardrails(agent.Guardrails{
			MaxCostUSD: next.Agent.MaxCostUSD,
			MaxTurns:   next.Agent.MaxTurns,
		})
	}

	if s.logger != nil {
		for _, k := range restart {
			s.logger.Warn("config.reload.restart_required", "field", k)
		}
	}

	return hot, nil
}

// configToSettings converts a *config.Config into AppSettings for validation.
func configToSettings(c *config.Config) AppSettings {
	return AppSettings{
		Agent:        c.Agent,
		Notification: c.Notification,
		Orchestrator: c.Orchestrator,
		Logging: LoggingSettings{
			Level:     c.Logging.Level,
			MaxSizeMB: c.Logging.MaxSizeMB,
			MaxFiles:  c.Logging.MaxFiles,
		},
		Audit:     c.Audit,
		Todoist:   c.Todoist,
		Renovate:  c.Renovate,
		Providers: c.Providers,
	}
}
