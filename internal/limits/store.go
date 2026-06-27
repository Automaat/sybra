package limits

import (
	"cmp"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
)

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
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	if p.Snapshots != nil {
		s.snapshots = p.Snapshots
	}
	for i := range p.Events {
		e := p.Events[i]
		if e.ID == "" {
			continue
		}
		if _, ok := s.seen[e.ID]; ok {
			continue
		}
		s.events = append(s.events, e)
		s.seen[e.ID] = struct{}{}
	}
	return s, nil
}

func (s *Store) UpdateSnapshot(snapshot Snapshot) error {
	if snapshot.Provider == "" {
		return nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.snapshots[snapshot.Provider]
	if ok && snapshot.CapturedAt.Before(prev.CapturedAt) {
		return nil
	}
	s.snapshots[snapshot.Provider] = snapshot
	return s.flushLocked()
}

func (s *Store) RecordUsage(e UsageEvent) error {
	if e.ID == "" || e.Provider == "" {
		return nil
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = s.now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[e.ID]; ok {
		return nil
	}
	s.events = append(s.events, e)
	s.seen[e.ID] = struct{}{}
	return s.flushLocked()
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
	providers := []string{ProviderClaude, ProviderCodex, ProviderCopilot}
	out := make([]ProviderSummary, 0, len(providers))
	for _, provider := range providers {
		snap := s.snapshots[provider]
		ps := ProviderSummary{
			Provider:               provider,
			PlanType:               snap.PlanType,
			LimitID:                snap.LimitID,
			Source:                 snap.Source,
			Confidence:             snap.Confidence,
			MonthlySubscriptionUSD: policy.SubscriptionMonthlyUSD[provider],
		}
		sessionStart, weeklyStart := fallbackWindows(now)
		if activeCycle(snap.Primary, now) {
			primary := snap.Primary
			ps.SessionUsedPercent = primary.UsedPercent
			ps.SessionWindowMinutes = primary.WindowMinutes
			ps.SessionResetsAt = primary.ResetsAt
			if !primary.ResetsAt.IsZero() && primary.WindowMinutes > 0 {
				sessionStart = primary.ResetsAt.Add(-time.Duration(primary.WindowMinutes) * time.Minute)
			}
		}
		if activeCycle(snap.Secondary, now) {
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
		ps.QuotaLimited, ps.QuotaReason = quotaLimited(ps, policy)
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
		return false, "provider disabled"
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

func quotaLimited(ps ProviderSummary, policy Policy) (limited bool, reason string) {
	if ps.Confidence != ConfidenceExact {
		return false, ""
	}
	if ps.SessionUsedPercent > 0 && ps.SessionUsedPercent >= policy.SessionThresholdPercent {
		return true, "session limit near threshold"
	}
	if ps.WeeklyUsedPercent > 0 && ps.WeeklyUsedPercent >= policy.WeeklyThresholdPercent {
		return true, "weekly limit near threshold"
	}
	return false, ""
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
