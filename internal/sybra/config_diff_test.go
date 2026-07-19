package sybra

import (
	"slices"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestDiffConfig_NoChange(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	got := diffConfig(*cfg, *cfg)
	if len(got.Applied) != 0 || len(got.RestartRequired) != 0 || len(got.Rejected) != 0 {
		t.Errorf("identical configs: got %+v, want empty change sets", got)
	}
}

func TestDiffConfig_HotOnly(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Agent.MaxConcurrent = 7
	next.Logging.Level = "debug"

	got := diffConfig(*old, next)

	if !slices.Contains(got.Applied, "agent") {
		t.Errorf("expected agent in applied, got %+v", got)
	}
	if !slices.Contains(got.Applied, "logging.level") {
		t.Errorf("expected logging.level in applied, got %+v", got)
	}
	if len(got.RestartRequired) != 0 {
		t.Errorf("expected no restart keys, got %+v", got)
	}
}

func TestDiffConfig_RestartOnly(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Providers.HealthCheck.IntervalSeconds = 600

	got := diffConfig(*old, next)

	if !slices.Contains(got.RestartRequired, "providers.health_check") {
		t.Errorf("expected providers.health_check in restart, got %+v", got)
	}
	if slices.Contains(got.Applied, "providers.health_check") {
		t.Errorf("providers.health_check must not appear in applied, got %+v", got)
	}
}

func TestDiffConfig_Mixed(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Agent.Provider = "codex"
	next.Monitor.Enabled = false

	got := diffConfig(*old, next)

	if !slices.Contains(got.Applied, "agent") {
		t.Errorf("expected agent in applied, got %+v", got)
	}
	if !slices.Contains(got.RestartRequired, "monitor") {
		t.Errorf("expected monitor in restart, got %+v", got)
	}
}

func TestDiffConfig_GuardrailsHot(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Agent.MaxCostUSD = 10.0
	next.Agent.MaxTurns = 200
	next.Agent.MaxCheckpoints = 7
	disabled := false
	next.Agent.CheckpointOnTurnCeiling = &disabled

	got := diffConfig(*old, next)

	if !slices.Contains(got.Applied, "agent") {
		t.Errorf("expected agent in applied, got %+v", got)
	}
}

func TestDiffConfig_MetricsRestart(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Metrics.Enabled = true

	got := diffConfig(*old, next)

	if !slices.Contains(got.RestartRequired, "metrics") {
		t.Errorf("expected metrics in restart, got %+v", got)
	}
}

func TestDiffConfig_BrowserRestart(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	disabled := false
	next.Browser.InApp = &disabled

	got := diffConfig(*old, next)
	if len(got.Applied) != 0 {
		t.Errorf("expected no applied keys, got %+v", got)
	}
	if !slices.Contains(got.RestartRequired, "browser") {
		t.Errorf("expected browser in restart, got %+v", got)
	}
}

func TestConfigRegistryCoversEveryConfigLeaf(t *testing.T) {
	t.Parallel()

	for _, path := range configRegistryCoveragePaths() {
		if !coveredByRegistry(path) {
			t.Fatalf("config leaf %q lacks reload metadata", path)
		}
	}
}
