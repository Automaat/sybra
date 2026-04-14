// Package metrics owns Synapse's OpenTelemetry metrics pipeline: meter
// provider setup, instrument definitions, and cheap record helpers called
// from subsystems. When metrics are disabled in config, Init is a no-op and
// every helper becomes a cheap nil check — subsystems do not need to branch.
//
// The Prometheus exporter registers instruments into the default
// prometheus/client_golang registry, so the HTTP handler is
// promhttp.Handler(). Scrapers hit /metrics on the synapse-server mux.
package metrics

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

const (
	meterName    = "github.com/Automaat/synapse"
	meterVersion = "0.1.0"
	serviceName  = "synapse"
)

var (
	initOnce sync.Once
	enabled  bool
	provider *sdkmetric.MeterProvider
	meter    metric.Meter

	// Instrument handles. Nil until Init succeeds with enabled=true.
	agentsStarted     metric.Int64Counter
	agentsCompleted   metric.Int64Counter
	agentDurationSecs metric.Float64Histogram

	tasksCreated metric.Int64Counter
	tasksUpdated metric.Int64Counter
	tasksDeleted metric.Int64Counter

	todoistPolls     metric.Int64Counter
	todoistImported  metric.Int64Counter
	todoistCompleted metric.Int64Counter

	githubFetches  metric.Int64Counter
	githubImported metric.Int64Counter

	renovatePolls metric.Int64Counter

	monitorTicks     metric.Int64Counter
	monitorAnomalies metric.Int64Counter

	orchestratorTicks         metric.Int64Counter
	orchestratorStaleRestarts metric.Int64Counter

	providerProbes        metric.Int64Counter
	providerHealthFlips   metric.Int64Counter
	providerAuthFailures  metric.Int64Counter
	providerRateLimits    metric.Int64Counter
	agentFailoversCounter metric.Int64Counter
	agentsGatedCounter    metric.Int64Counter

	// Observable gauge providers, mutated at wiring time and read from the
	// meter's registered callback. Guarded by obsMu.
	obsMu              sync.RWMutex
	tasksByStatusFn    func() map[string]int64
	agentsActiveFn     func() map[string]int64
	renovatePRsFetchFn func() int64
	providerHealthFn   func() map[string]int64
	tasksByStatusGauge metric.Int64ObservableGauge
	agentsActiveGauge  metric.Int64ObservableGauge
	renovatePRsGauge   metric.Int64ObservableGauge
	providerHealthyG   metric.Int64ObservableGauge
)

// Enabled reports whether the metrics pipeline was initialized and is active.
func Enabled() bool { return enabled }

// Init wires the OTel meter provider with a Prometheus exporter when
// cfg.Enabled is true. Subsequent calls are no-ops (safe to call from
// multiple entry points). Returns nil when disabled.
func Init(cfg config.MetricsConfig) error {
	var initErr error
	initOnce.Do(func() {
		if !cfg.Enabled {
			return
		}

		exporter, err := otelprom.New()
		if err != nil {
			initErr = fmt.Errorf("prometheus exporter: %w", err)
			return
		}

		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "unknown"
		}

		res, err := resource.Merge(
			resource.Default(),
			resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceName(serviceName),
				semconv.ServiceInstanceID(hostname),
				semconv.ServiceVersion(meterVersion),
			),
		)
		if err != nil {
			initErr = fmt.Errorf("resource merge: %w", err)
			return
		}

		provider = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(exporter),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(provider)
		meter = provider.Meter(meterName, metric.WithInstrumentationVersion(meterVersion))

		if err := createInstruments(); err != nil {
			initErr = fmt.Errorf("create instruments: %w", err)
			return
		}

		enabled = true
	})
	return initErr
}

// Shutdown flushes the meter provider. Safe to call when disabled.
func Shutdown(ctx context.Context) error {
	if provider == nil {
		return nil
	}
	return provider.Shutdown(ctx)
}

func createInstruments() error {
	if err := createAgentInstruments(); err != nil {
		return err
	}
	if err := createTaskInstruments(); err != nil {
		return err
	}
	if err := createPollInstruments(); err != nil {
		return err
	}
	if err := createOrchestratorInstruments(); err != nil {
		return err
	}
	if err := createProviderInstruments(); err != nil {
		return err
	}
	return createObservableGauges()
}

func createAgentInstruments() error {
	var err error
	if agentsStarted, err = meter.Int64Counter(
		"synapse_agents_started_total",
		metric.WithDescription("Count of agents started, by provider and mode."),
	); err != nil {
		return err
	}
	if agentsCompleted, err = meter.Int64Counter(
		"synapse_agents_completed_total",
		metric.WithDescription("Count of agents that reached a terminal state, by result."),
	); err != nil {
		return err
	}
	agentDurationSecs, err = meter.Float64Histogram(
		"synapse_agent_duration_seconds",
		metric.WithDescription("Agent wall-clock duration from start to terminal state."),
		metric.WithUnit("s"),
	)
	return err
}

func createTaskInstruments() error {
	var err error
	if tasksCreated, err = meter.Int64Counter(
		"synapse_tasks_created_total",
		metric.WithDescription("Tasks created via task.Manager."),
	); err != nil {
		return err
	}
	if tasksUpdated, err = meter.Int64Counter(
		"synapse_tasks_updated_total",
		metric.WithDescription("Tasks updated via task.Manager."),
	); err != nil {
		return err
	}
	tasksDeleted, err = meter.Int64Counter(
		"synapse_tasks_deleted_total",
		metric.WithDescription("Tasks deleted via task.Manager."),
	)
	return err
}

func createPollInstruments() error {
	var err error
	if todoistPolls, err = meter.Int64Counter(
		"synapse_todoist_polls_total",
		metric.WithDescription("Todoist poll attempts, by result."),
	); err != nil {
		return err
	}
	if todoistImported, err = meter.Int64Counter(
		"synapse_todoist_items_imported_total",
		metric.WithDescription("Todoist items imported as new tasks."),
	); err != nil {
		return err
	}
	if todoistCompleted, err = meter.Int64Counter(
		"synapse_todoist_items_completed_total",
		metric.WithDescription("Todoist items marked complete by Synapse."),
	); err != nil {
		return err
	}
	if githubFetches, err = meter.Int64Counter(
		"synapse_github_fetches_total",
		metric.WithDescription("GitHub Issues fetch attempts, by result."),
	); err != nil {
		return err
	}
	if githubImported, err = meter.Int64Counter(
		"synapse_github_issues_imported_total",
		metric.WithDescription("GitHub issues imported as tasks."),
	); err != nil {
		return err
	}
	renovatePolls, err = meter.Int64Counter(
		"synapse_renovate_polls_total",
		metric.WithDescription("Renovate PR poll attempts, by result."),
	)
	return err
}

func createOrchestratorInstruments() error {
	var err error
	if monitorTicks, err = meter.Int64Counter(
		"synapse_monitor_ticks_total",
		metric.WithDescription("Monitor service tick completions."),
	); err != nil {
		return err
	}
	if monitorAnomalies, err = meter.Int64Counter(
		"synapse_monitor_anomalies_total",
		metric.WithDescription("Monitor anomalies detected, by kind."),
	); err != nil {
		return err
	}
	if orchestratorTicks, err = meter.Int64Counter(
		"synapse_orchestrator_ticks_total",
		metric.WithDescription("Orchestrator loop iterations."),
	); err != nil {
		return err
	}
	orchestratorStaleRestarts, err = meter.Int64Counter(
		"synapse_orchestrator_stale_restarts_total",
		metric.WithDescription("Orchestrator stale in-progress task restart attempts, by result."),
	)
	return err
}

func createProviderInstruments() error {
	var err error
	if providerProbes, err = meter.Int64Counter(
		"synapse_provider_probes_total",
		metric.WithDescription("Provider health probe attempts, by provider and result."),
	); err != nil {
		return err
	}
	if providerHealthFlips, err = meter.Int64Counter(
		"synapse_provider_health_flips_total",
		metric.WithDescription("Provider health state flips, by provider and direction (healthy/unhealthy)."),
	); err != nil {
		return err
	}
	if providerAuthFailures, err = meter.Int64Counter(
		"synapse_provider_auth_failures_total",
		metric.WithDescription("Provider auth-failure signals reported by runners."),
	); err != nil {
		return err
	}
	if providerRateLimits, err = meter.Int64Counter(
		"synapse_provider_rate_limits_total",
		metric.WithDescription("Provider rate-limit signals reported by runners."),
	); err != nil {
		return err
	}
	if agentFailoversCounter, err = meter.Int64Counter(
		"synapse_agent_failovers_total",
		metric.WithDescription("Agent scheduling failovers triggered by an unhealthy provider."),
	); err != nil {
		return err
	}
	agentsGatedCounter, err = meter.Int64Counter(
		"synapse_agents_gated_total",
		metric.WithDescription("Agent runs refused by the provider health gate, by provider and reason."),
	)
	return err
}

func createObservableGauges() error {
	var err error
	if tasksByStatusGauge, err = meter.Int64ObservableGauge(
		"synapse_tasks_by_status",
		metric.WithDescription("Current task count grouped by status."),
	); err != nil {
		return err
	}
	if agentsActiveGauge, err = meter.Int64ObservableGauge(
		"synapse_agents_active",
		metric.WithDescription("Current agents grouped by state."),
	); err != nil {
		return err
	}
	if renovatePRsGauge, err = meter.Int64ObservableGauge(
		"synapse_renovate_prs_fetched",
		metric.WithDescription("Last observed count of open Renovate PRs."),
	); err != nil {
		return err
	}
	if providerHealthyG, err = meter.Int64ObservableGauge(
		"synapse_provider_healthy",
		metric.WithDescription("Current provider health (1=healthy, 0=unhealthy), by provider."),
	); err != nil {
		return err
	}
	_, err = meter.RegisterCallback(
		observe,
		tasksByStatusGauge, agentsActiveGauge, renovatePRsGauge, providerHealthyG,
	)
	return err
}

func observe(_ context.Context, obs metric.Observer) error {
	obsMu.RLock()
	byStatus := tasksByStatusFn
	byState := agentsActiveFn
	prs := renovatePRsFetchFn
	providerHealth := providerHealthFn
	obsMu.RUnlock()

	if byStatus != nil {
		for status, n := range byStatus() {
			obs.ObserveInt64(tasksByStatusGauge, n,
				metric.WithAttributes(attribute.String("status", status)))
		}
	}
	if byState != nil {
		for state, n := range byState() {
			obs.ObserveInt64(agentsActiveGauge, n,
				metric.WithAttributes(attribute.String("state", state)))
		}
	}
	if prs != nil {
		obs.ObserveInt64(renovatePRsGauge, prs())
	}
	if providerHealth != nil {
		for name, healthy := range providerHealth() {
			obs.ObserveInt64(providerHealthyG, healthy,
				metric.WithAttributes(attribute.String("provider", name)))
		}
	}
	return nil
}

// RegisterTasksByStatus wires a provider callback for the tasks_by_status
// observable gauge. Safe to call before or after Init; no-op when metrics
// are disabled.
func RegisterTasksByStatus(fn func() map[string]int64) {
	obsMu.Lock()
	tasksByStatusFn = fn
	obsMu.Unlock()
}

// RegisterAgentsActive wires a provider callback for agents_active.
func RegisterAgentsActive(fn func() map[string]int64) {
	obsMu.Lock()
	agentsActiveFn = fn
	obsMu.Unlock()
}

// RegisterRenovatePRsFetched wires a provider callback for
// renovate_prs_fetched.
func RegisterRenovatePRsFetched(fn func() int64) {
	obsMu.Lock()
	renovatePRsFetchFn = fn
	obsMu.Unlock()
}

// RegisterProviderHealth wires a provider callback returning provider name →
// health (1=healthy, 0=unhealthy). Invoked on every scrape.
func RegisterProviderHealth(fn func() map[string]int64) {
	obsMu.Lock()
	providerHealthFn = fn
	obsMu.Unlock()
}

// --- Record helpers. Each is a cheap nil guard when metrics are disabled. ---

func AgentStarted(provider, mode string) {
	if agentsStarted == nil {
		return
	}
	agentsStarted.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("mode", mode),
		))
}

func AgentCompleted(result string, dur time.Duration) {
	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String("result", result))
	if agentsCompleted != nil {
		agentsCompleted.Add(ctx, 1, attrs)
	}
	if agentDurationSecs != nil {
		agentDurationSecs.Record(ctx, dur.Seconds(), attrs)
	}
}

func TaskCreated() {
	if tasksCreated == nil {
		return
	}
	tasksCreated.Add(context.Background(), 1)
}

func TaskUpdated() {
	if tasksUpdated == nil {
		return
	}
	tasksUpdated.Add(context.Background(), 1)
}

func TaskDeleted() {
	if tasksDeleted == nil {
		return
	}
	tasksDeleted.Add(context.Background(), 1)
}

func TodoistPoll(ok bool) {
	if todoistPolls == nil {
		return
	}
	todoistPolls.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("result", resultLabel(ok))))
}

func TodoistImported(n int) {
	if todoistImported == nil || n <= 0 {
		return
	}
	todoistImported.Add(context.Background(), int64(n))
}

func TodoistCompleted(n int) {
	if todoistCompleted == nil || n <= 0 {
		return
	}
	todoistCompleted.Add(context.Background(), int64(n))
}

func GitHubFetch(ok bool) {
	if githubFetches == nil {
		return
	}
	githubFetches.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("result", resultLabel(ok))))
}

func GitHubIssuesImported(n int) {
	if githubImported == nil || n <= 0 {
		return
	}
	githubImported.Add(context.Background(), int64(n))
}

func RenovatePoll(ok bool) {
	if renovatePolls == nil {
		return
	}
	renovatePolls.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("result", resultLabel(ok))))
}

// MonitorTick records one completed tick of the in-process monitor service.
func MonitorTick() {
	if monitorTicks == nil {
		return
	}
	monitorTicks.Add(context.Background(), 1)
}

// MonitorAnomaly records one detected anomaly by kind.
func MonitorAnomaly(kind string) {
	if monitorAnomalies == nil {
		return
	}
	monitorAnomalies.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("kind", kind)))
}

func OrchestratorTick() {
	if orchestratorTicks == nil {
		return
	}
	orchestratorTicks.Add(context.Background(), 1)
}

func OrchestratorStaleRestart(ok bool) {
	if orchestratorStaleRestarts == nil {
		return
	}
	orchestratorStaleRestarts.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("result", resultLabel(ok))))
}

func ProviderProbe(name string, ok bool) {
	if providerProbes == nil {
		return
	}
	providerProbes.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("provider", name),
			attribute.String("result", resultLabel(ok)),
		))
}

// ProviderHealthFlip records a health state transition. direction is "healthy"
// or "unhealthy", matching the new state after the flip.
func ProviderHealthFlip(name string, healthy bool) {
	if providerHealthFlips == nil {
		return
	}
	direction := "unhealthy"
	if healthy {
		direction = "healthy"
	}
	providerHealthFlips.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("provider", name),
			attribute.String("direction", direction),
		))
}

func ProviderAuthFailure(name string) {
	if providerAuthFailures == nil {
		return
	}
	providerAuthFailures.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("provider", name)))
}

func ProviderRateLimit(name string) {
	if providerRateLimits == nil {
		return
	}
	providerRateLimits.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("provider", name)))
}

// AgentFailover records a scheduling failover from an unhealthy provider to a
// healthy peer.
func AgentFailover(from, to string) {
	if agentFailoversCounter == nil {
		return
	}
	agentFailoversCounter.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("from", from),
			attribute.String("to", to),
		))
}

// AgentGated records an agent run refused by the provider health gate.
func AgentGated(name, reason string) {
	if agentsGatedCounter == nil {
		return
	}
	agentsGatedCounter.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("provider", name),
			attribute.String("reason", reason),
		))
}

func resultLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}
