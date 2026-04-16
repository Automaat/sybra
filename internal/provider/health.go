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
	Interval         time.Duration
	ClaudeEnabled    bool
	CodexEnabled     bool
	AutoFailover     bool
	ClaudeRLCooldown time.Duration
	CodexRLCooldown  time.Duration
}

// HealthGate is the small surface the agent Manager depends on. Kept minimal
// so tests can supply a fake without spinning up a Checker.
type HealthGate interface {
	IsHealthy(provider string) bool
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

	emit   func(event string, data any)
	logger *slog.Logger

	probeClaude func(ctx context.Context) (Status, error)
	probeCodex  func(ctx context.Context) (Status, error)
	now         func() time.Time
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
	if logger == nil {
		logger = slog.Default()
	}
	if emit == nil {
		emit = func(string, any) {}
	}
	c := &Checker{
		cfg:         cfg,
		statuses:    make(map[string]*Status),
		emit:        emit,
		logger:      logger,
		probeClaude: ProbeClaude,
		probeCodex:  ProbeCodex,
		now:         time.Now,
	}
	// Seed defaults so Snapshot returns something meaningful before first probe.
	c.statuses["claude"] = &Status{Provider: "claude", Healthy: cfg.ClaudeEnabled, Reason: initialReason(cfg.ClaudeEnabled)}
	c.statuses["codex"] = &Status{Provider: "codex", Healthy: cfg.CodexEnabled, Reason: initialReason(cfg.CodexEnabled)}
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
	var claudeStatus, codexStatus Status
	var claudeErr, codexErr error
	var doClaude, doCodex bool

	c.mu.RLock()
	doClaude = c.cfg.ClaudeEnabled
	doCodex = c.cfg.CodexEnabled
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
	wg.Wait()

	if doClaude {
		c.applyProbeResult("claude", claudeStatus, claudeErr)
	}
	if doCodex {
		c.applyProbeResult("codex", codexStatus, codexErr)
	}
	c.clearExpiredRateLimits()
}

func (c *Checker) applyProbeResult(name string, result Status, err error) {
	metrics.ProviderProbe(name, err == nil)
	if err != nil {
		result = Status{
			Provider:  name,
			Healthy:   false,
			Reason:    "probe_error",
			Detail:    err.Error(),
			LastCheck: c.now(),
		}
	}
	if result.LastCheck.IsZero() {
		result.LastCheck = c.now()
	}
	c.setStatus(name, result, true)
}

// clearExpiredRateLimits walks the status map and flips providers back to
// healthy when their rate-limit window has elapsed. Active probes will
// eventually confirm state; this lets the gate release runs sooner.
func (c *Checker) clearExpiredRateLimits() {
	now := c.now()
	var toEmit []Status
	c.mu.Lock()
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
	c.mu.Unlock()
	for _, st := range toEmit {
		c.emitHealthEvent(st)
	}
}

// setStatus merges an incoming Status with the previous one and emits on flip.
// If fromProbe is false the input is a passive signal and only wins when it's
// strictly unhealthier than the existing state (so a stale probe-success does
// not wipe a real-time auth failure).
func (c *Checker) setStatus(name string, next Status, fromProbe bool) {
	c.mu.Lock()
	prev, ok := c.statuses[name]
	if !ok {
		prev = &Status{Provider: name}
		c.statuses[name] = prev
	}
	flip := false
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
			c.mu.Unlock()
			return
		}
		// Passive failures only upgrade severity; they never mark a provider
		// healthy and they never overwrite a more-recent probe result.
		if !prev.Healthy && prev.Reason == next.Reason && prev.RateLimitedUntil.Equal(next.RateLimitedUntil) {
			c.mu.Unlock()
			return
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
	c.mu.Unlock()

	if flip {
		metrics.ProviderHealthFlip(snapshot.Provider, snapshot.Healthy)
		c.logger.Info("provider.health.flip",
			"provider", snapshot.Provider,
			"healthy", snapshot.Healthy,
			"reason", snapshot.Reason)
		c.emitHealthEvent(snapshot)
	}
}

func statusChanged(a, b *Status) bool {
	return a.Healthy != b.Healthy ||
		a.Reason != b.Reason ||
		!a.RateLimitedUntil.Equal(b.RateLimitedUntil)
}

func (c *Checker) emitHealthEvent(s Status) {
	ev := HealthEvent{
		Provider:         s.Provider,
		Healthy:          s.Healthy,
		Reason:           s.Reason,
		Detail:           s.Detail,
		LastCheck:        s.LastCheck,
		RateLimitedUntil: s.RateLimitedUntil,
		FailoverActive:   c.failoverActive(s.Provider),
	}
	c.emit(ProviderHealthEvent, ev)
}

func (c *Checker) failoverActive(unhealthy string) bool {
	alt := c.Failover(unhealthy)
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

// Reason returns the current reason string for a provider, or empty if unknown.
func (c *Checker) Reason(provider string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.statuses[provider]; ok {
		return s.Reason
	}
	return ""
}

// Failover picks a healthy peer when auto-failover is enabled. Returns empty
// string if auto-failover is disabled or no peer is healthy.
func (c *Checker) Failover(unhealthy string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.cfg.AutoFailover {
		return ""
	}
	peer := otherProvider(unhealthy)
	if peer == "" {
		return ""
	}
	// Peer must be enabled and currently healthy.
	if (peer == "claude" && !c.cfg.ClaudeEnabled) || (peer == "codex" && !c.cfg.CodexEnabled) {
		return ""
	}
	if s, ok := c.statuses[peer]; ok && s.Healthy {
		return peer
	}
	return ""
}

func otherProvider(p string) string {
	switch p {
	case "claude":
		return "codex"
	case "codex":
		return "claude"
	default:
		return ""
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
	switch provider {
	case "claude":
		c.cfg.ClaudeEnabled = v
	case "codex":
		c.cfg.CodexEnabled = v
	default:
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
	snapshot := *s
	c.mu.Unlock()
	c.emitHealthEvent(snapshot)
}

// AutoFailover reports the current auto-failover flag.
func (c *Checker) AutoFailover() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.AutoFailover
}
