package limits

import (
	"cmp"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
)

const exactSnapshotMaxAge = 30 * time.Minute

// eventMaxAge bounds how long a UsageEvent survives in limits.json.
// Summary never looks back further than the weekly window (7d, see
// fallbackWindows) and BackfillLocalSessionFiles defaults to a 14d cutoff
// (ProviderLimitsConfig.BackfillDays) — 21d leaves a safety margin over both
// so a slow reload or clock skew can't prune data a consumer still needs.
// Without this, limits.json grows unbounded (56MB / ~189k events observed in
// production) since every RecordUsage/Import call only ever appends.
const eventMaxAge = 21 * 24 * time.Hour

type persisted struct {
	Snapshots map[string]Snapshot `json:"snapshots"`
	Events    []UsageEvent        `json:"events"`
}

// Store persists provider quota snapshots and local usage events.
type Store struct {
	path string
	now  func() time.Time

	mu        sync.Mutex
	snapshots map[string]Snapshot
	events    []UsageEvent
	seen      map[string]struct{}
}

func NewStore(path string) (*Store, error) {
	s := &Store{
		path:      path,
		now:       time.Now,
		snapshots: map[string]Snapshot{},
		seen:      map[string]struct{}{},
	}
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// UpdateSnapshot, RecordUsage, and Import are read-modify-write against the
// on-disk file: s.snapshots/s.events are populated once at NewStore time and
// otherwise never re-read, but sybra-cli and the GUI server each hold a
// separate Store over the same path in separate OS processes. Reloading
// under the cross-process flock immediately before mutating resyncs this
// process's view to the authoritative on-disk state, so a write from the
// other process in the gap since this process last loaded isn't clobbered.

func (s *Store) UpdateSnapshot(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := fsutil.LockFile(s.path)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	if !s.updateSnapshotLocked(snapshot) {
		return nil
	}
	return s.flushLocked()
}

func (s *Store) RecordUsage(e UsageEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := fsutil.LockFile(s.path)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	if !s.recordUsageLocked(e) {
		return nil
	}
	return s.flushLocked()
}

// Import records a batch of parsed session-file data and flushes at most once.
func (s *Store) Import(events []UsageEvent, snapshots []Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := fsutil.LockFile(s.path)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	changed := false
	for i := range snapshots {
		changed = s.updateSnapshotLocked(snapshots[i]) || changed
	}
	for i := range events {
		changed = s.recordUsageLocked(events[i]) || changed
	}
	if !changed {
		return nil
	}
	return s.flushLocked()
}

// InvalidateLiveExactSnapshot demotes a current live-poll snapshot to
// estimated confidence so routing falls back to event-based usage counters
// instead of trusting a now-unreadable exact quota sample.
func (s *Store) InvalidateLiveExactSnapshot(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := fsutil.LockFile(s.path)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	snap, ok := s.snapshots[provider]
	if !ok || snap.Source != SourceLivePoll || snap.Confidence != ConfidenceExact {
		return nil
	}
	snap.Confidence = ConfidenceEstimated
	s.snapshots[provider] = snap
	return s.flushLocked()
}

// reloadLocked re-reads s.path into s.snapshots/s.events/s.seen.
// Read-modify-write callers (UpdateSnapshot, RecordUsage, Import) must hold
// both s.mu and the cross-process file lock across the whole critical
// section; NewStore calls it unlocked because it runs before s is returned
// to any caller, so nothing else can observe or race it yet.
func (s *Store) reloadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.snapshots = map[string]Snapshot{}
			s.events = nil
			s.seen = map[string]struct{}{}
			return nil
		}
		return err
	}
	if len(data) == 0 {
		s.snapshots = map[string]Snapshot{}
		s.events = nil
		s.seen = map[string]struct{}{}
		return nil
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	s.snapshots = map[string]Snapshot{}
	if p.Snapshots != nil {
		s.snapshots = p.Snapshots
	}
	s.events = nil
	s.seen = map[string]struct{}{}
	cutoff := s.now().UTC().Add(-eventMaxAge)
	for i := range p.Events {
		e := p.Events[i]
		if e.ID == "" {
			continue
		}
		if e.Timestamp.Before(cutoff) {
			continue
		}
		if _, ok := s.seen[e.ID]; ok {
			continue
		}
		s.events = append(s.events, e)
		s.seen[e.ID] = struct{}{}
	}
	return nil
}

func (s *Store) updateSnapshotLocked(snapshot Snapshot) bool {
	if snapshot.Provider == "" {
		return false
	}
	if snapshot.CapturedAt.IsZero() {
		snapshot.CapturedAt = s.now().UTC()
	}
	if snapshot.Source == "" {
		snapshot.Source = SourceStream
	}
	if snapshot.Confidence == "" {
		snapshot.Confidence = ConfidenceExact
	}
	prev, ok := s.snapshots[snapshot.Provider]
	if ok && snapshot.CapturedAt.Before(prev.CapturedAt) {
		return false
	}
	s.snapshots[snapshot.Provider] = snapshot
	return true
}

func (s *Store) recordUsageLocked(e UsageEvent) bool {
	if e.ID == "" || e.Provider == "" {
		return false
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = s.now().UTC()
	}
	if _, ok := s.seen[e.ID]; ok {
		return false
	}
	s.events = append(s.events, e)
	s.seen[e.ID] = struct{}{}
	return true
}

func (s *Store) Snapshot(provider string) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.snapshots[provider]
	return v, ok
}

func (s *Store) Summary(policy Policy) Summary {
	if policy.SessionThresholdPercent <= 0 {
		policy.SessionThresholdPercent = 85
	}
	if policy.WeeklyThresholdPercent <= 0 {
		policy.WeeklyThresholdPercent = 90
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	providers := []string{ProviderClaude, ProviderCodex, ProviderCopilot, ProviderOpenCode}
	out := make([]ProviderSummary, 0, len(providers))
	for _, provider := range providers {
		snap := s.snapshots[provider]
		snapshotFresh := freshExactSnapshot(snap, now)
		confidence := snap.Confidence
		if snap.Confidence == ConfidenceExact && !snapshotFresh {
			confidence = ConfidenceEstimated
		}
		ps := ProviderSummary{
			Provider:               provider,
			PlanType:               snap.PlanType,
			LimitID:                snap.LimitID,
			Source:                 snap.Source,
			Confidence:             confidence,
			MonthlySubscriptionUSD: policy.SubscriptionMonthlyUSD[provider],
		}
		sessionStart, weeklyStart := fallbackWindows(now)
		if snapshotFresh && activeCycle(snap.Primary, now) {
			primary := snap.Primary
			ps.SessionUsedPercent = primary.UsedPercent
			ps.SessionWindowMinutes = primary.WindowMinutes
			ps.SessionResetsAt = primary.ResetsAt
			if !primary.ResetsAt.IsZero() && primary.WindowMinutes > 0 {
				sessionStart = primary.ResetsAt.Add(-time.Duration(primary.WindowMinutes) * time.Minute)
			}
		}
		if snapshotFresh && activeCycle(snap.Secondary, now) {
			secondary := snap.Secondary
			ps.WeeklyUsedPercent = secondary.UsedPercent
			ps.WeeklyWindowMinutes = secondary.WindowMinutes
			ps.WeeklyResetsAt = secondary.ResetsAt
			if !secondary.ResetsAt.IsZero() && secondary.WindowMinutes > 0 {
				weeklyStart = secondary.ResetsAt.Add(-time.Duration(secondary.WindowMinutes) * time.Minute)
			}
		}
		sessionUsageSource := usageCounterSource(s.events, provider, sessionStart)
		weeklyUsageSource := usageCounterSource(s.events, provider, weeklyStart)
		for i := range s.events {
			e := &s.events[i]
			if e.Provider != provider {
				continue
			}
			if !e.Timestamp.Before(sessionStart) {
				addEventToProviderSummary(e, &ps, true, e.Source == sessionUsageSource)
			}
			if !e.Timestamp.Before(weeklyStart) {
				addEventToProviderSummary(e, &ps, false, e.Source == weeklyUsageSource)
			}
		}
		ps.QuotaLimited, ps.QuotaReason = quotaLimited(ps, snap, policy)
		if ps.MonthlySubscriptionUSD > 0 {
			ps.MonthlySubscriptionBurnRate = ps.WeeklySpendUSD / ps.MonthlySubscriptionUSD
		}
		out = append(out, ps)
	}
	return Summary{Providers: out, UpdatedAt: now}
}

// ProviderAvailable reports whether the latest exact quota state is below the
// configured thresholds. Estimated providers are left available; their usage
// still affects scoring but not hard blocking.
func (s *Store) ProviderAvailable(provider string, policy Policy) (available bool, reason string) {
	if !providerEnabled(policy, provider) {
		return false, quotaReasonProviderDisabled
	}
	if !policy.Enabled {
		return true, ""
	}
	summary := s.Summary(policy)
	for i := range summary.Providers {
		p := &summary.Providers[i]
		if p.Provider != provider {
			continue
		}
		if p.QuotaLimited {
			return false, p.QuotaReason
		}
		return true, ""
	}
	return true, ""
}

func (s *Store) ChooseProvider(requested string, candidates []string, healthy func(string) bool, policy Policy) (provider, reason string) {
	if !policy.Enabled && providerEnabled(policy, requested) {
		return "", ""
	}
	summary := s.Summary(policy)
	byProvider := map[string]*ProviderSummary{}
	for i := range summary.Providers {
		p := &summary.Providers[i]
		byProvider[p.Provider] = p
	}
	requestedSummary := byProvider[requested]
	var requestedPressure float64
	var requestedLimited bool
	if requestedSummary != nil {
		requestedPressure = maxFloat(requestedSummary.SessionUsedPercent, requestedSummary.WeeklyUsedPercent)
		requestedLimited = requestedSummary.QuotaLimited
	}
	available := make([]*ProviderSummary, 0, len(candidates))
	for _, p := range candidates {
		if p == requested || !providerEnabled(policy, p) || !healthy(p) {
			continue
		}
		ps := byProvider[p]
		if ps == nil {
			ps = &ProviderSummary{Provider: p}
		}
		if ps.QuotaLimited {
			continue
		}
		available = append(available, ps)
	}
	if len(available) == 0 {
		return "", ""
	}
	if policy.PreferUnderused {
		slices.SortFunc(available, func(a, b *ProviderSummary) int {
			au := maxFloat(a.SessionUsedPercent, a.WeeklyUsedPercent)
			bu := maxFloat(b.SessionUsedPercent, b.WeeklyUsedPercent)
			if au != bu {
				return cmp.Compare(au, bu)
			}
			return cmp.Compare(a.WeeklySpendUSD, b.WeeklySpendUSD)
		})
	}
	if !requestedLimited {
		// Do not route away from the requested/default provider just because
		// another provider has no data. Prefer-underused should react to real
		// pressure in an exact quota snapshot.
		if requestedSummary == nil || requestedSummary.Confidence != ConfidenceExact || requestedPressure < 70 {
			return "", ""
		}
		altPressure := maxFloat(available[0].SessionUsedPercent, available[0].WeeklyUsedPercent)
		if available[0].Confidence == ConfidenceExact && altPressure+10 < requestedPressure {
			return available[0].Provider, "lower quota pressure"
		}
		return "", ""
	}
	return available[0].Provider, "lower quota pressure"
}

// ChooseSoftLimitedPeer finds a peer that is only soft-threshold limited
// (session/weekly near threshold, not a hard rate-limit-reached block or a
// disabled provider) to use as a last-resort failover target when the
// requested provider is itself hard-blocked and ChooseProvider found no
// fully available peer. This mirrors the leniency softLimitLastResort
// already grants a provider's own soft-threshold state, extended to peers:
// a peer that still safely dispatches its own direct runs under a soft
// threshold is an acceptable failover target too, rather than failing the
// dispatch closed system-wide.
func (s *Store) ChooseSoftLimitedPeer(requested string, candidates []string, healthy func(string) bool, policy Policy) (provider, reason string) {
	if !policy.Enabled {
		return "", ""
	}
	summary := s.Summary(policy)
	byProvider := map[string]*ProviderSummary{}
	for i := range summary.Providers {
		p := &summary.Providers[i]
		byProvider[p.Provider] = p
	}
	for _, p := range candidates {
		if p == requested || !providerEnabled(policy, p) || !healthy(p) {
			continue
		}
		ps := byProvider[p]
		if ps == nil || !ps.QuotaLimited {
			// Fully available peers are already handled by ChooseProvider;
			// reaching here means none existed.
			continue
		}
		if IsSoftThresholdReason(ps.QuotaReason) {
			return p, ps.QuotaReason
		}
	}
	return "", ""
}

func (s *Store) flushLocked() error {
	data, err := json.Marshal(persisted{Snapshots: s.snapshots, Events: s.events})
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.path, data)
}

func fallbackWindows(now time.Time) (sessionStart, weeklyStart time.Time) {
	return now.Add(-5 * time.Hour), now.AddDate(0, 0, -7)
}

func activeCycle(c *CycleSnapshot, now time.Time) bool {
	return c != nil && (c.ResetsAt.IsZero() || c.ResetsAt.After(now))
}

func freshExactSnapshot(snap Snapshot, now time.Time) bool {
	if snap.Confidence != ConfidenceExact || snap.CapturedAt.IsZero() {
		return false
	}
	return !snap.CapturedAt.Before(now.Add(-exactSnapshotMaxAge))
}

func providerEnabled(policy Policy, provider string) bool {
	if len(policy.ProviderEnabled) == 0 {
		return true
	}
	enabled, ok := policy.ProviderEnabled[provider]
	return ok && enabled
}

func usageCounterSource(events []UsageEvent, provider string, start time.Time) string {
	for i := range events {
		e := &events[i]
		if e.Provider == provider && e.Source == SourceSessionFiles && !e.Timestamp.Before(start) {
			return SourceSessionFiles
		}
	}
	return SourceRunStats
}

func addEventToProviderSummary(e *UsageEvent, ps *ProviderSummary, session, addCounters bool) {
	if session {
		ps.SessionSpendUSD += e.CostUSD
		ps.SessionPremiumRequests += e.PremiumRequests
		if !addCounters {
			return
		}
		ps.SessionInputTokens += e.InputTokens
		ps.SessionOutputTokens += e.OutputTokens
		ps.SessionCacheReadTokens += e.CacheReadInputTokens
		ps.SessionReasoningTokens += e.ReasoningTokens
		return
	}
	ps.WeeklySpendUSD += e.CostUSD
	ps.WeeklyPremiumRequests += e.PremiumRequests
	if !addCounters {
		return
	}
	ps.WeeklyInputTokens += e.InputTokens
	ps.WeeklyOutputTokens += e.OutputTokens
	ps.WeeklyCacheReadTokens += e.CacheReadInputTokens
	ps.WeeklyReasoningTokens += e.ReasoningTokens
}

const (
	quotaReasonRateLimitReached = "provider reports rate limit reached"
	quotaReasonSessionThreshold = "session limit near threshold"
	quotaReasonWeeklyThreshold  = "weekly limit near threshold"
	quotaReasonProviderDisabled = "provider disabled"
)

// IsSoftThresholdReason reports whether a ProviderAvailable "unavailable" reason
// is a soft reserve threshold (session/weekly near threshold) rather than a hard
// block. A soft threshold should only redirect a run to a healthier peer; it
// must never strand a task on a provider that still has budget when no peer is
// available. Hard blocks (rate limit actually reached, provider disabled) return
// false so they keep gating.
func IsSoftThresholdReason(reason string) bool {
	return reason == quotaReasonSessionThreshold || reason == quotaReasonWeeklyThreshold
}

// IsRateLimitReachedReason reports whether a ProviderAvailable "unavailable"
// reason is a hard rate-limit-reached block. Unlike provider-disabled (a config
// kill-switch that stays until a human re-enables it), a reached rate limit is
// transient and self-heals when the quota window rolls over, so callers mark it
// on the gate error to keep it off the dispatch circuit breaker.
func IsRateLimitReachedReason(reason string) bool {
	return reason == quotaReasonRateLimitReached
}

func quotaLimited(ps ProviderSummary, snap Snapshot, policy Policy) (limited bool, reason string) {
	if ps.Confidence != ConfidenceExact {
		return false, ""
	}
	if strings.TrimSpace(snap.RateLimitReachedType) != "" {
		return true, quotaReasonRateLimitReached
	}
	if ps.SessionUsedPercent > 0 && ps.SessionUsedPercent >= policy.SessionThresholdPercent {
		return true, quotaReasonSessionThreshold
	}
	if ps.WeeklyUsedPercent > 0 && ps.WeeklyUsedPercent >= policy.WeeklyThresholdPercent {
		return true, quotaReasonWeeklyThreshold
	}
	return false, ""
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
