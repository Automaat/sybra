package provider

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/metrics"
)

// Status is a point-in-time health snapshot for a single provider.
type Status struct {
	Provider         string    `json:"provider"`
	Healthy          bool      `json:"healthy"`
	Reason           string    `json:"reason"`
	Detail           string    `json:"detail,omitempty"`
	LastCheck        time.Time `json:"lastCheck"`
	RateLimitedUntil time.Time `json:"ratelimitedUntil,omitzero"`
}

// Config controls the Checker's probe schedule and failover policy.
type Config struct {
	Interval          time.Duration
	ClaudeEnabled     bool
	CodexEnabled      bool
	CopilotEnabled    bool
	AutoFailover      bool
	ClaudeRLCooldown  time.Duration
	CodexRLCooldown   time.Duration
	CopilotRLCooldown time.Duration
	// ProbeErrorThreshold is the number of consecutive generic probe_error
	// results required before a provider is marked unhealthy. Auth and
	// rate-limit states still apply immediately.
	ProbeErrorThreshold int
}

// failoverPriority is the order auto-failover prefers healthy peers in.
// Copilot is last: it is never chosen over claude/codex, but can be a
// fallback target when both are unhealthy, and can itself fail over to them.
var failoverPriority = []string{"claude", "codex", "copilot"}

const defaultProbeErrorThreshold = 2

// HealthGate is the small surface the agent Manager depends on. Kept minimal
// so tests can supply a fake without spinning up a Checker.
type HealthGate interface {
	IsHealthy(provider string) bool
	// RateLimited reports whether the provider is specifically in a rate-limit
	// cooldown window (as opposed to logged out / auth-failed). Lets callers
	// wait-and-retry a transient throttle without also waiting on auth failures
	// that need human login.
	RateLimited(provider string) bool
	Failover(unhealthy string) string
	Reason(provider string) string
	ReportAuthFailure(provider, reason string)
	ReportRateLimit(provider string, retryAfter time.Duration, reason string)
}

// HealthEvent is the payload emitted on state flips to the frontend.
type HealthEvent struct {
	Provider         string    `json:"provider"`
	Healthy          bool      `json:"healthy"`
	Reason           string    `json:"reason"`
	Detail           string    `json:"detail,omitempty"`
	LastCheck        time.Time `json:"lastCheck"`
	RateLimitedUntil time.Time `json:"ratelimitedUntil,omitzero"`
	FailoverActive   bool      `json:"failoverActive"`
}

// ProviderHealthEvent is the wails event name for health state flips. Kept in
// this package so the constant lives next to the payload shape.
const ProviderHealthEvent = "provider:health"

// Checker holds provider health state and runs active probes on a ticker.
// It satisfies HealthGate.
type Checker struct {
	mu       sync.RWMutex
	cfg      Config
	statuses map[string]*Status
	// probeFailures tracks consecutive generic probe_error results. The first
	// one is treated as a soft failure so a transient local CLI hiccup does not
	// flash a global provider outage or gate otherwise-working agents.
	probeFailures map[string]int

	emit   func(event string, data any)
	logger *slog.Logger

	probeClaude  func(ctx context.Context) (Status, error)
	probeCodex   func(ctx context.Context) (Status, error)
	probeCopilot func(ctx context.Context) (Status, error)
	now          func() time.Time
}

// New constructs a Checker. Zero-value config fields are filled with defaults.
func New(cfg Config, emit func(string, any), logger *slog.Logger) *Checker {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.ClaudeRLCooldown <= 0 {
		cfg.ClaudeRLCooldown = 15 * time.Minute
	}
	if cfg.CodexRLCooldown <= 0 {
		cfg.CodexRLCooldown = 15 * time.Minute
	}
	if cfg.CopilotRLCooldown <= 0 {
		cfg.CopilotRLCooldown = 15 * time.Minute
	}
	if cfg.ProbeErrorThreshold <= 0 {
		cfg.ProbeErrorThreshold = defaultProbeErrorThreshold
	}
	if logger == nil {
		logger = slog.Default()
	}
	if emit == nil {
		emit = func(string, any) {}
	}
	c := &Checker{
		cfg:           cfg,
		statuses:      make(map[string]*Status),
		probeFailures: make(map[string]int),
		emit:          emit,
		logger:        logger,
		probeClaude:   ProbeClaude,
		probeCodex:    ProbeCodex,
		probeCopilot:  ProbeCopilot,
		now:           time.Now,
	}
	// Seed defaults so Snapshot returns something meaningful before first probe.
	c.statuses["claude"] = &Status{Provider: "claude", Healthy: cfg.ClaudeEnabled, Reason: initialReason(cfg.ClaudeEnabled)}
	c.statuses["codex"] = &Status{Provider: "codex", Healthy: cfg.CodexEnabled, Reason: initialReason(cfg.CodexEnabled)}
	c.statuses["copilot"] = &Status{Provider: "copilot", Healthy: cfg.CopilotEnabled, Reason: initialReason(cfg.CopilotEnabled)}
	return c
}

func initialReason(enabled bool) string {
	if enabled {
		return "unknown"
	}
	return "disabled"
}

// Run performs an immediate probe and then probes on a ticker until ctx is
// cancelled. Safe to call once from a goroutine.
func (c *Checker) Run(ctx context.Context) {
	c.checkAll(ctx)
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.checkAll(ctx)
		}
	}
}

func (c *Checker) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	var claudeStatus, codexStatus, copilotStatus Status
	var claudeErr, codexErr, copilotErr error
	var doClaude, doCodex, doCopilot bool

	c.mu.RLock()
	doClaude = c.cfg.ClaudeEnabled
	doCodex = c.cfg.CodexEnabled
	doCopilot = c.cfg.CopilotEnabled
	c.mu.RUnlock()

	if doClaude {
		wg.Go(func() {
			claudeStatus, claudeErr = c.probeClaude(ctx)
		})
	}
	if doCodex {
		wg.Go(func() {
			codexStatus, codexErr = c.probeCodex(ctx)
		})
	}
	if doCopilot {
		wg.Go(func() {
			copilotStatus, copilotErr = c.probeCopilot(ctx)
		})
	}
	wg.Wait()

	results := make([]probeResult, 0, 3)
	if doClaude {
		results = append(results, probeResult{provider: "claude", status: claudeStatus, err: claudeErr})
	}
	if doCodex {
		results = append(results, probeResult{provider: "codex", status: codexStatus, err: codexErr})
	}
	if doCopilot {
		results = append(results, probeResult{provider: "copilot", status: copilotStatus, err: copilotErr})
	}
	c.applyProbeResults(results)
}

type probeResult struct {
	provider string
	status   Status
	err      error
}

func (c *Checker) applyProbeResults(results []probeResult) {
	if len(results) == 0 {
		return
	}
	for i := range results {
		metrics.ProviderProbe(results[i].provider, results[i].err == nil)
	}

	var events []HealthEvent
	c.mu.Lock()
	var flips []Status
	for i := range results {
		status, ok := c.normalizeProbeResultLocked(results[i])
		if !ok {
			continue
		}
		if snapshot, flip := c.setStatusLocked(results[i].provider, status, true); flip {
			flips = append(flips, snapshot)
		}
	}
	flips = append(flips, c.clearExpiredRateLimitsLocked(c.now())...)
	for _, st := range flips {
		events = append(events, c.healthEventLocked(st))
	}
	c.mu.Unlock()

	c.emitHealthEvents(events)
}

func (c *Checker) normalizeProbeResultLocked(r probeResult) (Status, bool) {
	result := r.status
	if r.err != nil {
		result = Status{
			Provider:  r.provider,
			Healthy:   false,
			Reason:    "probe_error",
			Detail:    r.err.Error(),
			LastCheck: c.now(),
		}
	}
	if result.Provider == "" {
		result.Provider = r.provider
	}
	if result.LastCheck.IsZero() {
		result.LastCheck = c.now()
	}
	if !result.Healthy && result.Reason == "probe_error" {
		c.probeFailures[r.provider]++
		if c.probeFailures[r.provider] < c.cfg.ProbeErrorThreshold {
			c.logger.Info("provider.probe.transient",
				"provider", r.provider,
				"failures", c.probeFailures[r.provider],
				"threshold", c.cfg.ProbeErrorThreshold,
				"detail", result.Detail)
			// A probe did run — advance LastCheck on the stored status so
			// Snapshot/UX don't look like probing stalled, while leaving the
			// debounced Healthy/Reason untouched until the threshold trips.
			if prev, ok := c.statuses[r.provider]; ok {
				prev.LastCheck = c.now()
			}
			return Status{}, false
		}
	} else {
		c.probeFailures[r.provider] = 0
	}
	return result, true
}

// clearExpiredRateLimits walks the status map and flips providers back to
// healthy when their rate-limit window has elapsed. Active probes will
// eventually confirm state; this lets the gate release runs sooner.
func (c *Checker) clearExpiredRateLimits() {
	var events []HealthEvent
	c.mu.Lock()
	flips := c.clearExpiredRateLimitsLocked(c.now())
	for _, st := range flips {
		events = append(events, c.healthEventLocked(st))
	}
	c.mu.Unlock()
	c.emitHealthEvents(events)
}

func (c *Checker) clearExpiredRateLimitsLocked(now time.Time) []Status {
	var toEmit []Status
	for _, s := range c.statuses {
		if !s.RateLimitedUntil.IsZero() && now.After(s.RateLimitedUntil) {
			s.RateLimitedUntil = time.Time{}
			if s.Reason == "rate_limited" {
				s.Healthy = true
				s.Reason = "ok"
				s.Detail = "rate_limit_window_expired"
				s.LastCheck = now
				toEmit = append(toEmit, *s)
			}
		}
	}
	return toEmit
}

// setStatus merges an incoming Status with the previous one and emits on flip.
// If fromProbe is false the input is a passive signal and only wins when it's
// strictly unhealthier than the existing state (so a stale probe-success does
// not wipe a real-time auth failure).
func (c *Checker) setStatus(name string, next Status, fromProbe bool) {
	c.mu.Lock()
	snapshot, flip := c.setStatusLocked(name, next, fromProbe)
	var ev HealthEvent
	if flip {
		ev = c.healthEventLocked(snapshot)
	}
	c.mu.Unlock()

	if flip {
		c.emitHealthEvents([]HealthEvent{ev})
	}
}

func (c *Checker) setStatusLocked(name string, next Status, fromProbe bool) (Status, bool) {
	prev, ok := c.statuses[name]
	if !ok {
		prev = &Status{Provider: name}
		c.statuses[name] = prev
	}
	if !fromProbe {
		c.probeFailures[name] = 0
	}
	var flip bool
	if fromProbe {
		// Active probes overwrite unconditionally, but preserve an in-flight
		// rate-limit window when the probe still reports healthy — the window
		// should be cleared by clearExpiredRateLimits or a newer passive signal.
		if next.Healthy && !prev.RateLimitedUntil.IsZero() && c.now().Before(prev.RateLimitedUntil) {
			next.Healthy = false
			next.Reason = "rate_limited"
			next.RateLimitedUntil = prev.RateLimitedUntil
		}
		flip = statusChanged(prev, &next)
		*prev = next
	} else {
		if next.Healthy {
			// A passive "healthy" signal doesn't exist — guard anyway.
			return Status{}, false
		}
		// Passive failures only upgrade severity; they never mark a provider
		// healthy and they never overwrite a more-recent probe result.
		if !prev.Healthy && prev.Reason == next.Reason && prev.RateLimitedUntil.Equal(next.RateLimitedUntil) {
			return Status{}, false
		}
		prev.Healthy = false
		prev.Reason = next.Reason
		prev.Detail = next.Detail
		prev.LastCheck = next.LastCheck
		if !next.RateLimitedUntil.IsZero() {
			prev.RateLimitedUntil = next.RateLimitedUntil
		}
		flip = true
	}
	snapshot := *prev
	return snapshot, flip
}

func statusChanged(a, b *Status) bool {
	return a.Healthy != b.Healthy ||
		a.Reason != b.Reason ||
		!a.RateLimitedUntil.Equal(b.RateLimitedUntil)
}

func (c *Checker) healthEventLocked(s Status) HealthEvent {
	return HealthEvent{
		Provider:         s.Provider,
		Healthy:          s.Healthy,
		Reason:           s.Reason,
		Detail:           s.Detail,
		LastCheck:        s.LastCheck,
		RateLimitedUntil: s.RateLimitedUntil,
		FailoverActive:   c.failoverActiveLocked(s.Provider),
	}
}

func (c *Checker) emitHealthEvents(events []HealthEvent) {
	for _, ev := range events {
		metrics.ProviderHealthFlip(ev.Provider, ev.Healthy)
		args := []any{
			"provider", ev.Provider,
			"healthy", ev.Healthy,
			"reason", ev.Reason,
		}
		if ev.Detail != "" {
			args = append(args, "detail", ev.Detail)
		}
		c.logger.Info("provider.health.flip", args...)
		c.emit(ProviderHealthEvent, ev)
	}
}

func (c *Checker) failoverActiveLocked(unhealthy string) bool {
	alt := c.failoverLocked(unhealthy)
	return alt != "" && alt != unhealthy
}

// --- HealthGate implementation ---

// IsHealthy reports whether the named provider can currently be used.
func (c *Checker) IsHealthy(provider string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.statuses[provider]
	if !ok {
		return true
	}
	return s.Healthy
}

// RateLimited reports whether the provider is currently inside a rate-limit
// cooldown window. Only ReportRateLimit sets RateLimitedUntil, so an auth
// failure (logged out) returns false here even though IsHealthy is false —
// that's the distinction recovery uses to wait-and-retry rate limits while
// letting auth failures take the human-required path.
func (c *Checker) RateLimited(provider string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.statuses[provider]
	if !ok {
		return false
	}
	return !s.RateLimitedUntil.IsZero() && c.now().Before(s.RateLimitedUntil)
}

// Reason returns the current reason string for a provider, or empty if unknown.
func (c *Checker) Reason(provider string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.statuses[provider]; ok {
		return s.Reason
	}
	return ""
}

// Failover picks a healthy peer when auto-failover is enabled, walking
// failoverPriority in order (claude > codex > copilot). Returns empty string
// if auto-failover is disabled or no enabled peer is currently healthy.
func (c *Checker) Failover(unhealthy string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.failoverLocked(unhealthy)
}

func (c *Checker) failoverLocked(unhealthy string) string {
	if !c.cfg.AutoFailover {
		return ""
	}
	for _, peer := range failoverPriority {
		if peer == unhealthy || !c.providerEnabledLocked(peer) {
			continue
		}
		if s, ok := c.statuses[peer]; ok && s.Healthy {
			return peer
		}
	}
	return ""
}

// providerEnabledLocked reports whether a provider participates in probing and
// failover. Caller must hold c.mu.
func (c *Checker) providerEnabledLocked(provider string) bool {
	switch provider {
	case "claude":
		return c.cfg.ClaudeEnabled
	case "codex":
		return c.cfg.CodexEnabled
	case "copilot":
		return c.cfg.CopilotEnabled
	default:
		return false
	}
}

// ReportAuthFailure marks a provider as logged-out from a passive runner signal.
// Only cleared by a successful active probe.
func (c *Checker) ReportAuthFailure(provider, reason string) {
	metrics.ProviderAuthFailure(provider)
	if reason == "" {
		reason = "logged_out"
	}
	c.setStatus(provider, Status{
		Provider:  provider,
		Healthy:   false,
		Reason:    reason,
		LastCheck: c.now(),
	}, false)
}

// ReportRateLimit marks a provider as rate-limited. retryAfter zero falls back
// to the per-provider configured cooldown.
func (c *Checker) ReportRateLimit(provider string, retryAfter time.Duration, reason string) {
	metrics.ProviderRateLimit(provider)
	cooldown := retryAfter
	if cooldown <= 0 {
		c.mu.RLock()
		switch provider {
		case "claude":
			cooldown = c.cfg.ClaudeRLCooldown
		case "codex":
			cooldown = c.cfg.CodexRLCooldown
		case "copilot":
			cooldown = c.cfg.CopilotRLCooldown
		default:
			cooldown = 15 * time.Minute
		}
		c.mu.RUnlock()
	}
	if reason == "" {
		reason = "rate_limited"
	}
	until := c.now().Add(cooldown)
	c.setStatus(provider, Status{
		Provider:         provider,
		Healthy:          false,
		Reason:           "rate_limited",
		Detail:           reason,
		LastCheck:        c.now(),
		RateLimitedUntil: until,
	}, false)
}

// Snapshot returns a copy of the current statuses for wails-bound read paths.
func (c *Checker) Snapshot() map[string]Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Status, len(c.statuses))
	for k, v := range c.statuses {
		out[k] = *v
	}
	return out
}

// SetAutoFailover toggles auto-failover at runtime (from Settings UI).
func (c *Checker) SetAutoFailover(v bool) {
	c.mu.Lock()
	c.cfg.AutoFailover = v
	c.mu.Unlock()
}

// SetProviderEnabled toggles per-provider probing and participation in failover.
func (c *Checker) SetProviderEnabled(provider string, v bool) {
	c.mu.Lock()
	var prev bool
	switch provider {
	case "claude":
		prev, c.cfg.ClaudeEnabled = c.cfg.ClaudeEnabled, v
	case "codex":
		prev, c.cfg.CodexEnabled = c.cfg.CodexEnabled, v
	case "copilot":
		prev, c.cfg.CopilotEnabled = c.cfg.CopilotEnabled, v
	default:
		c.mu.Unlock()
		return
	}
	if prev == v {
		// No-op toggle — don't emit a spurious health flip event.
		c.mu.Unlock()
		return
	}
	s, ok := c.statuses[provider]
	if !ok {
		s = &Status{Provider: provider}
		c.statuses[provider] = s
	}
	if !v {
		s.Healthy = false
		s.Reason = "disabled"
	} else if s.Reason == "disabled" {
		s.Reason = "unknown"
	}
	c.probeFailures[provider] = 0
	snapshot := *s
	ev := c.healthEventLocked(snapshot)
	c.mu.Unlock()
	c.emitHealthEvents([]HealthEvent{ev})
}

// AutoFailover reports the current auto-failover flag.
func (c *Checker) AutoFailover() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.AutoFailover
}
