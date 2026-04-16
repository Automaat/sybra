package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
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
		"sybra_agents_started_total",
		"sybra_agents_completed_total",
		"sybra_agent_duration_seconds",
		"sybra_tasks_created_total",
		"sybra_tasks_updated_total",
		"sybra_tasks_deleted_total",
		"sybra_todoist_polls_total",
		"sybra_todoist_items_imported_total",
		"sybra_todoist_items_completed_total",
		"sybra_github_fetches_total",
		"sybra_github_issues_imported_total",
		"sybra_renovate_polls_total",
		"sybra_monitor_ticks_total",
		"sybra_monitor_anomalies_total",
		"sybra_orchestrator_ticks_total",
		"sybra_orchestrator_stale_restarts_total",
		"sybra_tasks_by_status",
		"sybra_agents_active",
		"sybra_renovate_prs_fetched",
		"sybra_provider_probes_total",
		"sybra_provider_health_flips_total",
		"sybra_provider_auth_failures_total",
		"sybra_provider_rate_limits_total",
		"sybra_agent_failovers_total",
		"sybra_agents_gated_total",
		"sybra_provider_healthy",
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
