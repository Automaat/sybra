package agent

import (
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/limits"
)

// Adversarial probes for Manager.ProviderHealthy, independent of the
// implementer's own manager_test.go additions.
func TestAdversarial_ProviderHealthy_EnabledButProbeUnhealthy(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		Runtime: ManagerRuntimeConfig{
			LimitPolicy: limits.Policy{
				ProviderEnabled: map[string]bool{
					"copilot": true,
				},
			},
		},
	})
	// Config says enabled=true, but the probe-based health gate says down.
	// Both checks must be honored - config enabled does not override a real
	// outage.
	m.SetHealthGate(stubGate{rateLimited: true})
	if m.ProviderHealthy("copilot") {
		t.Error("provider enabled=true in config but probe-unhealthy should still report false")
	}
}

func TestAdversarial_ProviderHealthy_AbsentFromMapButProbeUnhealthy(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		Runtime: ManagerRuntimeConfig{
			LimitPolicy: limits.Policy{
				ProviderEnabled: map[string]bool{
					"claude": true,
				},
			},
		},
	})
	m.SetHealthGate(stubGate{rateLimited: true})
	if m.ProviderHealthy("copilot") {
		t.Error("provider absent from ProviderEnabled map but probe-unhealthy should still report false")
	}
}

func TestAdversarial_ProviderHealthy_EmptyNameResolvesDefault(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		Runtime: ManagerRuntimeConfig{
			DefaultProvider: "claude",
			LimitPolicy: limits.Policy{
				ProviderEnabled: map[string]bool{
					"claude": false,
				},
			},
		},
	})
	if m.ProviderHealthy("") {
		t.Error("empty provider name should resolve to config-disabled default provider and report false")
	}
}

func TestAdversarial_ProviderHealthy_ConcurrentAccess(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		Runtime: ManagerRuntimeConfig{
			LimitPolicy: limits.Policy{
				ProviderEnabled: map[string]bool{
					"copilot": false,
					"claude":  true,
				},
			},
		},
	})
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = m.ProviderHealthy("copilot")
		}()
		go func() {
			defer wg.Done()
			m.SetHealthGate(stubGate{rateLimited: false})
		}()
	}
	wg.Wait()
	if m.ProviderHealthy("copilot") {
		t.Error("copilot should remain unhealthy under concurrent access (config-disabled)")
	}
}
