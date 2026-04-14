package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// TestMetricsPipeline verifies both the disabled path (pre-Init helpers are
// nil-safe no-ops) and the enabled path (each record helper produces output
// visible through promhttp.Handler). Init uses sync.Once, so this single
// test covers both modes without cross-test contamination.
func TestMetricsPipeline(t *testing.T) {
	// Pre-Init: every helper must be a cheap nil-check no-op.
	if Enabled() {
		t.Fatal("Enabled() returned true before Init")
	}
	AgentStarted("claude", "headless")
	TaskCreated()
	TodoistPoll(true)
	GitHubFetch(false)
	RenovatePoll(true)
	MonitorTick()
	MonitorAnomaly("lost_agent")
	OrchestratorTick()
	ProviderProbe("claude", true)
	ProviderHealthFlip("claude", false)
	ProviderAuthFailure("claude")
	ProviderRateLimit("codex")
	AgentFailover("claude", "codex")
	AgentGated("claude", "logged_out")

	if err := Init(config.MetricsConfig{Enabled: true}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !Enabled() {
		t.Fatal("Enabled() returned false after successful Init")
	}

	// Exercise every helper so each instrument has at least one sample.
	AgentStarted("claude", "headless")
	AgentCompleted("ok", 2*time.Second)
	AgentCompleted("error", 500*time.Millisecond)
	TaskCreated()
	TaskCreated()
	TaskUpdated()
	TaskDeleted()
	TodoistPoll(true)
	TodoistPoll(false)
	TodoistImported(3)
	TodoistCompleted(1)
	GitHubFetch(true)
	GitHubIssuesImported(7)
	RenovatePoll(true)
	MonitorTick()
	MonitorTick()
	MonitorAnomaly("lost_agent")
	MonitorAnomaly("pr_gap")
	OrchestratorTick()
	OrchestratorStaleRestart(true)
	OrchestratorStaleRestart(false)
	ProviderProbe("claude", true)
	ProviderProbe("codex", false)
	ProviderHealthFlip("claude", false)
	ProviderHealthFlip("claude", true)
	ProviderAuthFailure("claude")
	ProviderRateLimit("codex")
	AgentFailover("claude", "codex")
	AgentGated("claude", "logged_out")

	// Observable gauges — provide live callbacks.
	RegisterTasksByStatus(func() map[string]int64 {
		return map[string]int64{"todo": 3, "done": 5}
	})
	RegisterAgentsActive(func() map[string]int64 {
		return map[string]int64{"running": 2, "idle": 1}
	})
	RegisterRenovatePRsFetched(func() int64 { return 6 })
	RegisterProviderHealth(func() map[string]int64 {
		return map[string]int64{"claude": 1, "codex": 0}
	})

	body := scrape(t)

	wantSubstrings := []string{
		"synapse_agents_started_total",
		"synapse_agents_completed_total",
		"synapse_agent_duration_seconds",
		"synapse_tasks_created_total",
		"synapse_tasks_updated_total",
		"synapse_tasks_deleted_total",
		"synapse_todoist_polls_total",
		"synapse_todoist_items_imported_total",
		"synapse_todoist_items_completed_total",
		"synapse_github_fetches_total",
		"synapse_github_issues_imported_total",
		"synapse_renovate_polls_total",
		"synapse_monitor_ticks_total",
		"synapse_monitor_anomalies_total",
		"synapse_orchestrator_ticks_total",
		"synapse_orchestrator_stale_restarts_total",
		"synapse_tasks_by_status",
		"synapse_agents_active",
		"synapse_renovate_prs_fetched",
		"synapse_provider_probes_total",
		"synapse_provider_health_flips_total",
		"synapse_provider_auth_failures_total",
		"synapse_provider_rate_limits_total",
		"synapse_agent_failovers_total",
		"synapse_agents_gated_total",
		"synapse_provider_healthy",
		`status="todo"`,
		`state="running"`,
		`result="ok"`,
		`result="error"`,
		`kind="lost_agent"`,
		`kind="pr_gap"`,
		`provider="claude"`,
		`from="claude"`,
		`to="codex"`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("scrape output missing %q", want)
		}
	}
}

func scrape(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
