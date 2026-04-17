package sybra

import (
	"slices"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestDiffConfig_NoChange(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	hot, restart := diffConfig(*cfg, *cfg)
	if len(hot) != 0 || len(restart) != 0 {
		t.Errorf("identical configs: hot=%v restart=%v, want empty", hot, restart)
	}
}

func TestDiffConfig_HotOnly(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Agent.MaxConcurrent = 7
	next.Logging.Level = "debug"

	hot, restart := diffConfig(*old, next)

	if !slices.Contains(hot, "agent.max_concurrent") {
		t.Errorf("expected agent.max_concurrent in hot, got %v", hot)
	}
	if !slices.Contains(hot, "logging.level") {
		t.Errorf("expected logging.level in hot, got %v", hot)
	}
	if len(restart) != 0 {
		t.Errorf("expected no restart keys, got %v", restart)
	}
}

func TestDiffConfig_RestartOnly(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Providers.HealthCheck.IntervalSeconds = 600

	hot, restart := diffConfig(*old, next)

	if !slices.Contains(restart, "providers") {
		t.Errorf("expected providers in restart, got %v", restart)
	}
	if slices.Contains(hot, "providers") {
		t.Errorf("providers must not appear in hot, got %v", hot)
	}
}

func TestDiffConfig_Mixed(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Agent.Provider = "codex"
	next.Monitor.Enabled = false

	hot, restart := diffConfig(*old, next)

	if !slices.Contains(hot, "agent.provider") {
		t.Errorf("expected agent.provider in hot, got %v", hot)
	}
	if !slices.Contains(restart, "monitor") {
		t.Errorf("expected monitor in restart, got %v", restart)
	}
}

func TestDiffConfig_TodoistBlock(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Todoist.APIToken = "tok-123"
	next.Todoist.Enabled = true

	hot, _ := diffConfig(*old, next)

	if !slices.Contains(hot, "todoist") {
		t.Errorf("expected todoist in hot, got %v", hot)
	}
}

func TestDiffConfig_GuardrailsHot(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Agent.MaxCostUSD = 10.0
	next.Agent.MaxTurns = 200

	hot, _ := diffConfig(*old, next)

	if !slices.Contains(hot, "agent.max_cost_usd") {
		t.Errorf("expected agent.max_cost_usd in hot, got %v", hot)
	}
	if !slices.Contains(hot, "agent.max_turns") {
		t.Errorf("expected agent.max_turns in hot, got %v", hot)
	}
}

func TestDiffConfig_MetricsRestart(t *testing.T) {
	t.Parallel()
	old := config.DefaultConfig()
	next := *old
	next.Metrics.Enabled = true

	hot, restart := diffConfig(*old, next)
	_ = hot

	if !slices.Contains(restart, "metrics") {
		t.Errorf("expected metrics in restart, got %v", restart)
	}
}
