package sybra

import "github.com/Automaat/sybra/internal/config"

// ReloadFromDisk re-reads ~/.sybra/config.yaml, validates it, diffs against
// the persisted intent snapshot, and applies only hot-reloadable changes.
// Restart-required values stay pending until process restart. Never writes to disk.
func (s *ConfigService) ReloadFromDisk() (ConfigMutationResult, error) {
	next, err := config.LoadNoPersist()
	if err != nil {
		return ConfigMutationResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutateLocked(next, nil)
}

// configToSettings converts a *config.Config into AppSettings for validation
// and for the default-settings baseline the UI diffs against. It mirrors every
// editable section GetSettings exposes.
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
		Attachments: c.Attachments,
		Audit:       c.Audit,
		Renovate:    c.Renovate,
		Providers:   c.Providers,
		ProviderRouting: ProviderRoutingSettings{
			ABTestingEnabled:              c.ABTesting.Enabled,
			ABTestingMinSamplesPerVariant: c.ABTesting.MinSamplesPerVariant,
			Summary:                       config.BuildRoutingSummary(c),
		},
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
	}
}
