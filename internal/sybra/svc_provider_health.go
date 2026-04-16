package sybra

import (
	"fmt"

	"github.com/Automaat/sybra/internal/provider"
)

// GetProviderHealth returns the current health snapshot for all providers as
// a slice (wails generates typed bindings from slice-returning methods more
// reliably than from map returns). Empty when health check is disabled.
func (s *IntegrationService) GetProviderHealth() []provider.Status {
	if s.providerHealth == nil {
		return []provider.Status{}
	}
	snap := s.providerHealth.Snapshot()
	out := make([]provider.Status, 0, len(snap))
	for _, st := range snap {
		out = append(out, st)
	}
	return out
}

// ProviderHealthEnabled reports whether the background health check loop is
// active. The frontend uses this to decide whether to show the Providers
// Settings section.
func (s *IntegrationService) ProviderHealthEnabled() bool {
	return s.providerHealth != nil
}

// SetProviderAutoFailover toggles auto-failover at runtime and persists the
// change to disk.
func (s *IntegrationService) SetProviderAutoFailover(enabled bool) error {
	if s.providerHealth == nil {
		return fmt.Errorf("provider health check disabled")
	}
	s.cfg.Providers.AutoFailover = enabled
	s.providerHealth.SetAutoFailover(enabled)
	if s.saveConfig != nil {
		if err := s.saveConfig(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
	}
	return nil
}

// SetProviderEnabled toggles per-provider probing and persists the change.
func (s *IntegrationService) SetProviderEnabled(name string, enabled bool) error {
	if s.providerHealth == nil {
		return fmt.Errorf("provider health check disabled")
	}
	switch name {
	case "claude":
		s.cfg.Providers.Claude.Enabled = enabled
	case "codex":
		s.cfg.Providers.Codex.Enabled = enabled
	default:
		return fmt.Errorf("unknown provider %q", name)
	}
	s.providerHealth.SetProviderEnabled(name, enabled)
	if s.saveConfig != nil {
		if err := s.saveConfig(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
	}
	return nil
}
