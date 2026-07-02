package limits

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCodexLine_RateLimitsAndUsage(t *testing.T) {
	line := []byte(`{"timestamp":"2026-06-19T12:40:08.052Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":23846,"cached_input_tokens":8576,"output_tokens":438,"reasoning_output_tokens":213,"total_tokens":24284},"last_token_usage":{"input_tokens":23846,"cached_input_tokens":8576,"output_tokens":438,"reasoning_output_tokens":213,"total_tokens":24284},"model_context_window":258400},"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":81.5,"window_minutes":300,"resets_at":1781877547},"secondary":{"used_percent":64.0,"window_minutes":10080,"resets_at":1782380122},"credits":null,"individual_limit":null,"plan_type":"prolite","rate_limit_reached_type":null}}}`)

	snapshot, event, ok := ParseCodexLine(line, SourceSessionFiles, "id1", "sess1")
	if !ok {
		t.Fatal("ParseCodexLine returned ok=false")
	}
	if snapshot.Provider != ProviderCodex || snapshot.Confidence != ConfidenceExact {
		t.Fatalf("snapshot provider/confidence = %q/%q", snapshot.Provider, snapshot.Confidence)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 81.5 || snapshot.Primary.WindowMinutes != 300 {
		t.Fatalf("primary snapshot mismatch: %+v", snapshot.Primary)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.UsedPercent != 64.0 || snapshot.Secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary snapshot mismatch: %+v", snapshot.Secondary)
	}
	if event.ID != "id1" || event.SessionID != "sess1" || event.InputTokens != 23846 || event.CacheReadInputTokens != 8576 {
		t.Fatalf("usage event mismatch: %+v", event)
	}
}

func TestParseClaudeUsageSnapshot(t *testing.T) {
	now := time.Date(2026, 6, 28, 8, 45, 0, 0, time.UTC)
	line := []byte(`{"five_hour":{"utilization":0.0,"resets_at":null},"seven_day":{"utilization":100.0,"resets_at":"2026-07-01T14:59:59.747459+00:00"}}`)

	snapshot, ok, err := parseClaudeUsageSnapshot(line, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("parseClaudeUsageSnapshot returned ok=false")
	}
	if snapshot.Provider != ProviderClaude || snapshot.Source != SourceLivePoll || snapshot.Confidence != ConfidenceExact {
		t.Fatalf("snapshot provider/source/confidence mismatch: %+v", snapshot)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 0 || snapshot.Primary.WindowMinutes != 300 {
		t.Fatalf("primary snapshot mismatch: %+v", snapshot.Primary)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.UsedPercent != 100 || snapshot.Secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary snapshot mismatch: %+v", snapshot.Secondary)
	}
	if snapshot.Secondary.ResetsAt.IsZero() {
		t.Fatalf("secondary reset was not parsed: %+v", snapshot.Secondary)
	}
}

func TestParseCodexAppServerSnapshot(t *testing.T) {
	now := time.Date(2026, 6, 28, 8, 45, 0, 0, time.UTC)
	line := []byte(`{"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":9,"windowDurationMins":300,"resetsAt":1782652179},"secondary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":1782989755},"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"individualLimit":null,"planType":"prolite","rateLimitReachedType":"rate_limit_reached"}}`)

	snapshot, ok, err := parseCodexAppServerSnapshot(line, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("parseCodexAppServerSnapshot returned ok=false")
	}
	if snapshot.Provider != ProviderCodex || snapshot.Source != SourceLivePoll || snapshot.Confidence != ConfidenceExact {
		t.Fatalf("snapshot provider/source/confidence mismatch: %+v", snapshot)
	}
	if snapshot.PlanType != "prolite" || snapshot.RateLimitReachedType != "rate_limit_reached" {
		t.Fatalf("metadata mismatch: %+v", snapshot)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 9 || snapshot.Primary.WindowMinutes != 300 {
		t.Fatalf("primary snapshot mismatch: %+v", snapshot.Primary)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.UsedPercent != 100 || snapshot.Secondary.WindowMinutes != 10080 {
		t.Fatalf("secondary snapshot mismatch: %+v", snapshot.Secondary)
	}
}

func TestStoreProviderAvailableAndChooseProvider(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	policy := DefaultPolicy()

	if err := s.UpdateSnapshot(Snapshot{
		Provider:   ProviderCodex,
		Source:     SourceStream,
		Confidence: ConfidenceExact,
		CapturedAt: now,
		Primary:    &CycleSnapshot{UsedPercent: 91, WindowMinutes: 300, ResetsAt: now.Add(time.Hour)},
		Secondary:  &CycleSnapshot{UsedPercent: 50, WindowMinutes: 10080, ResetsAt: now.AddDate(0, 0, 3)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSnapshot(Snapshot{
		Provider:   ProviderClaude,
		Source:     SourceRunStats,
		Confidence: ConfidenceEstimated,
		CapturedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	ok, reason := s.ProviderAvailable(ProviderCodex, policy)
	if ok || reason == "" {
		t.Fatalf("codex available = %v, reason=%q; want limited", ok, reason)
	}
	alt, _ := s.ChooseProvider(ProviderCodex, []string{ProviderClaude, ProviderCodex, ProviderCopilot}, func(string) bool { return true }, policy)
	if alt != ProviderClaude {
		t.Fatalf("alternative = %q, want claude", alt)
	}
}

func TestSummary_DowngradesStaleExactSnapshot(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := s.UpdateSnapshot(Snapshot{
		Provider:   ProviderCodex,
		Source:     SourceSessionFiles,
		Confidence: ConfidenceExact,
		CapturedAt: now.Add(-2 * time.Hour),
		Primary:    &CycleSnapshot{UsedPercent: 10, WindowMinutes: 300, ResetsAt: now.Add(time.Hour)},
		Secondary:  &CycleSnapshot{UsedPercent: 68, WindowMinutes: 10080, ResetsAt: now.AddDate(0, 0, 4)},
	}); err != nil {
		t.Fatal(err)
	}

	summary := s.Summary(DefaultPolicy())
	var codex ProviderSummary
	for _, p := range summary.Providers {
		if p.Provider == ProviderCodex {
			codex = p
			break
		}
	}
	if codex.Confidence != ConfidenceEstimated {
		t.Fatalf("confidence = %q, want estimated for stale snapshot", codex.Confidence)
	}
	if codex.SessionUsedPercent != 0 || codex.WeeklyUsedPercent != 0 {
		t.Fatalf("stale exact percentages surfaced: %+v", codex)
	}
	if codex.QuotaLimited {
		t.Fatalf("stale exact snapshot limited provider: %+v", codex)
	}
}

func TestSummary_RateLimitReachedTypeLimitsProvider(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := s.UpdateSnapshot(Snapshot{
		Provider:             ProviderCodex,
		Source:               SourceLivePoll,
		Confidence:           ConfidenceExact,
		CapturedAt:           now,
		RateLimitReachedType: "rate_limit_reached",
		Primary:              &CycleSnapshot{UsedPercent: 9, WindowMinutes: 300, ResetsAt: now.Add(time.Hour)},
		Secondary:            &CycleSnapshot{UsedPercent: 100, WindowMinutes: 10080, ResetsAt: now.AddDate(0, 0, 4)},
	}); err != nil {
		t.Fatal(err)
	}

	ok, reason := s.ProviderAvailable(ProviderCodex, DefaultPolicy())
	if ok || reason != "provider reports rate limit reached" {
		t.Fatalf("ProviderAvailable = %v, reason=%q; want provider-reported limit", ok, reason)
	}
}

func TestStoreProviderAvailable_IgnoresExpiredQuotaCycle(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := s.UpdateSnapshot(Snapshot{
		Provider:   ProviderCodex,
		Source:     SourceStream,
		Confidence: ConfidenceExact,
		CapturedAt: now.Add(-6 * time.Hour),
		Primary:    &CycleSnapshot{UsedPercent: 99, WindowMinutes: 300, ResetsAt: now.Add(-time.Minute)},
		Secondary:  &CycleSnapshot{UsedPercent: 10, WindowMinutes: 10080, ResetsAt: now.AddDate(0, 0, 3)},
	}); err != nil {
		t.Fatal(err)
	}
	summary := s.Summary(DefaultPolicy())
	var codex ProviderSummary
	for _, p := range summary.Providers {
		if p.Provider == ProviderCodex {
			codex = p
			break
		}
	}
	if codex.SessionUsedPercent != 0 || codex.SessionResetsAt != (time.Time{}) {
		t.Fatalf("expired session cycle still surfaced: %+v", codex)
	}
	if codex.QuotaLimited {
		t.Fatalf("expired session cycle still limits provider: %+v", codex)
	}
	if ok, reason := s.ProviderAvailable(ProviderCodex, DefaultPolicy()); !ok {
		t.Fatalf("ProviderAvailable = false, reason=%q; want available after reset", reason)
	}
}

func TestChooseProvider_SkipsPolicyDisabledCandidates(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := s.UpdateSnapshot(Snapshot{
		Provider:   ProviderClaude,
		Source:     SourceStream,
		Confidence: ConfidenceExact,
		CapturedAt: now,
		Primary:    &CycleSnapshot{UsedPercent: 95, WindowMinutes: 300, ResetsAt: now.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	policy := DefaultPolicy()
	policy.ProviderEnabled[ProviderCodex] = false

	alt, _ := s.ChooseProvider(ProviderClaude, []string{ProviderClaude, ProviderCodex, ProviderCopilot}, func(string) bool {
		return true
	}, policy)
	if alt == ProviderCodex {
		t.Fatalf("ChooseProvider selected disabled provider %q", alt)
	}
	if alt != ProviderCopilot {
		t.Fatalf("alternative = %q, want copilot as only enabled candidate", alt)
	}
}

func TestChooseProvider_NoDataDoesNotStealRequested(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	alt, _ := s.ChooseProvider(ProviderClaude, []string{ProviderClaude, ProviderCodex}, func(string) bool { return true }, DefaultPolicy())
	if alt != "" {
		t.Fatalf("alternative = %q, want none without quota pressure", alt)
	}
}

func TestSummary_PrefersSessionFileUsageCountersButKeepsRunSpend(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	if err := s.RecordUsage(UsageEvent{
		ID:                   "session-file",
		Provider:             ProviderCodex,
		Source:               SourceSessionFiles,
		InputTokens:          100,
		OutputTokens:         20,
		CacheReadInputTokens: 40,
		ReasoningTokens:      5,
		Timestamp:            now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(UsageEvent{
		ID:                   "sybra-run",
		Provider:             ProviderCodex,
		Source:               SourceRunStats,
		CostUSD:              1.25,
		InputTokens:          100,
		OutputTokens:         20,
		CacheReadInputTokens: 40,
		ReasoningTokens:      5,
		PremiumRequests:      2,
		Timestamp:            now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	summary := s.Summary(DefaultPolicy())
	var codex ProviderSummary
	for _, p := range summary.Providers {
		if p.Provider == ProviderCodex {
			codex = p
			break
		}
	}
	if codex.SessionInputTokens != 100 || codex.WeeklyInputTokens != 100 {
		t.Fatalf("input tokens = session %d weekly %d, want 100/100", codex.SessionInputTokens, codex.WeeklyInputTokens)
	}
	if codex.SessionOutputTokens != 20 || codex.SessionCacheReadTokens != 40 || codex.SessionReasoningTokens != 5 {
		t.Fatalf("session counters double counted: %+v", codex)
	}
	if codex.SessionSpendUSD != 1.25 || codex.WeeklySpendUSD != 1.25 {
		t.Fatalf("spend = session %.2f weekly %.2f, want 1.25/1.25", codex.SessionSpendUSD, codex.WeeklySpendUSD)
	}
	if codex.SessionPremiumRequests != 2 || codex.WeeklyPremiumRequests != 2 {
		t.Fatalf("premium requests = session %v weekly %v, want 2/2", codex.SessionPremiumRequests, codex.WeeklyPremiumRequests)
	}
}

func TestStoreImport_DedupesAndPersistsBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	if err := s.Import(
		[]UsageEvent{
			{ID: "claude-1", Provider: ProviderClaude, Source: SourceSessionFiles, InputTokens: 10},
			{ID: "claude-1", Provider: ProviderClaude, Source: SourceSessionFiles, InputTokens: 99},
			{ID: "codex-1", Provider: ProviderCodex, Source: SourceSessionFiles, InputTokens: 20, Timestamp: now.Add(-time.Minute)},
		},
		[]Snapshot{
			{Provider: ProviderCodex, Source: SourceSessionFiles, Confidence: ConfidenceExact, PlanType: "new", CapturedAt: now},
			{Provider: ProviderCodex, Source: SourceSessionFiles, Confidence: ConfidenceExact, PlanType: "old", CapturedAt: now.Add(-time.Hour)},
		},
	); err != nil {
		t.Fatal(err)
	}

	if len(s.events) != 2 {
		t.Fatalf("events = %d, want 2 after duplicate import", len(s.events))
	}
	if s.events[0].Timestamp != now {
		t.Fatalf("zero timestamp was not normalized: %v", s.events[0].Timestamp)
	}
	snap, ok := s.Snapshot(ProviderCodex)
	if !ok || snap.PlanType != "new" {
		t.Fatalf("snapshot = %+v, ok=%v; want latest snapshot", snap, ok)
	}

	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.events) != 2 {
		t.Fatalf("persisted events = %d, want 2", len(reopened.events))
	}
	snap, ok = reopened.Snapshot(ProviderCodex)
	if !ok || snap.PlanType != "new" {
		t.Fatalf("persisted snapshot = %+v, ok=%v; want latest snapshot", snap, ok)
	}
}

// TestRecordUsageCrossProcessSimulatesConcurrentWriters models two OS
// processes (e.g. the GUI server and sybra-cli) each holding their own
// *Store over the same path. s.events/s.snapshots are loaded once at
// NewStore time and never re-read on the query paths, so without
// RecordUsage reloading from disk under the flock before recording, s2's
// write would overwrite s1's not-yet-visible-to-s2 event entirely instead of
// merging with it.
func TestRecordUsageCrossProcessSimulatesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.json")

	s1, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := s1.RecordUsage(UsageEvent{ID: "e1", Provider: ProviderClaude, Timestamp: time.Now()}); err != nil {
		t.Fatalf("s1.RecordUsage: %v", err)
	}
	if err := s2.RecordUsage(UsageEvent{ID: "e2", Provider: ProviderCodex, Timestamp: time.Now()}); err != nil {
		t.Fatalf("s2.RecordUsage: %v", err)
	}

	s3, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s3.events) != 2 {
		t.Fatalf("events = %d, want 2 — an event was dropped by concurrent cross-process writes", len(s3.events))
	}
}

func TestSessionImport_DedupesEventsAndKeepsLatestSnapshot(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	batch := newSessionImport()

	batch.addEvent(UsageEvent{ID: "event-1", Provider: ProviderClaude, Source: SourceSessionFiles, InputTokens: 1})
	batch.addEvent(UsageEvent{ID: "event-1", Provider: ProviderClaude, Source: SourceSessionFiles, InputTokens: 2})
	batch.addEvent(UsageEvent{ID: "", Provider: ProviderClaude, Source: SourceSessionFiles})

	batch.addSnapshot(Snapshot{Provider: ProviderCodex, PlanType: "new", CapturedAt: now})
	batch.addSnapshot(Snapshot{Provider: ProviderCodex, PlanType: "old", CapturedAt: now.Add(-time.Hour)})

	if len(batch.events) != 1 {
		t.Fatalf("events = %d, want 1", len(batch.events))
	}
	snapshots := batch.snapshotsList()
	if len(snapshots) != 1 || snapshots[0].PlanType != "new" {
		t.Fatalf("snapshots = %+v, want latest only", snapshots)
	}
}

func TestBackfillLocalSessionFiles_StopsOnCanceledContext(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = s.BackfillLocalSessionFiles(ctx, time.Time{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BackfillLocalSessionFiles error = %v, want context.Canceled", err)
	}
}

func TestWalkJSONL_StopsOnCanceledContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "session.jsonl"), []byte(`{"type":"message"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	err := walkJSONL(ctx, root, time.Time{}, func(string, int64, []byte) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("walkJSONL error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("walkJSONL callback ran after context cancellation")
	}
}
