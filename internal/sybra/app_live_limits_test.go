package sybra

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

func TestLiveLimitPollState_ClaudeAuthBackoffAndRecovery(t *testing.T) {
	limitStore, err := limits.NewStore(filepath.Join(t.TempDir(), "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 22, 0, 0, 0, time.UTC)
	if err := limitStore.UpdateSnapshot(limits.Snapshot{
		Provider:   limits.ProviderClaude,
		Source:     limits.SourceLivePoll,
		Confidence: limits.ConfidenceExact,
		CapturedAt: now,
		Primary:    &limits.CycleSnapshot{UsedPercent: 91, WindowMinutes: 300, ResetsAt: now.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	records := make([]slog.Record, 0)
	logger := slog.New(&recordHandler{records: &records})
	state := newLiveLimitPollState(now)

	authErr := fmt.Errorf("claude: %w", &limits.LivePollError{
		Provider:   limits.ProviderClaude,
		Kind:       limits.LivePollErrorKindAuth,
		StatusCode: 401,
		Op:         "fetch Claude usage snapshot",
	})
	state.recordResult(now, limits.LiveRefreshResult{
		Providers: map[string]limits.LiveProviderResult{
			limits.ProviderClaude: {Attempted: true, Err: authErr},
		},
	}, limitStore, logger)

	if !state.claudeAuthOpen || state.claudeAuthFailures != 1 {
		t.Fatalf("auth state after first failure = open:%v failures:%d", state.claudeAuthOpen, state.claudeAuthFailures)
	}
	if got := state.next[limits.ProviderClaude]; !got.Equal(now.Add(liveLimitPollInterval)) {
		t.Fatalf("first backoff next poll = %v, want %v", got, now.Add(liveLimitPollInterval))
	}
	snap, ok := limitStore.Snapshot(limits.ProviderClaude)
	if !ok || snap.Confidence != limits.ConfidenceEstimated {
		t.Fatalf("snapshot after invalidation = %+v, ok=%v", snap, ok)
	}

	secondNow := now.Add(liveLimitPollInterval)
	state.recordResult(secondNow, limits.LiveRefreshResult{
		Providers: map[string]limits.LiveProviderResult{
			limits.ProviderClaude: {Attempted: true, Err: authErr},
		},
	}, limitStore, logger)

	if state.claudeAuthFailures != 2 {
		t.Fatalf("auth failures after second failure = %d, want 2", state.claudeAuthFailures)
	}
	if got := state.next[limits.ProviderClaude]; !got.Equal(secondNow.Add(2 * liveLimitPollInterval)) {
		t.Fatalf("second backoff next poll = %v, want %v", got, secondNow.Add(2*liveLimitPollInterval))
	}
	if got := countLogMessage(records, "limits.live_poll.claude_auth"); got != 1 {
		t.Fatalf("auth warning count = %d, want 1 deduped log", got)
	}

	recoveredAt := secondNow.Add(2 * liveLimitPollInterval)
	state.recordResult(recoveredAt, limits.LiveRefreshResult{
		Providers: map[string]limits.LiveProviderResult{
			limits.ProviderClaude: {Attempted: true, Snapshot: true},
		},
	}, limitStore, logger)

	if state.claudeAuthOpen || state.claudeAuthFailures != 0 {
		t.Fatalf("auth state after recovery = open:%v failures:%d", state.claudeAuthOpen, state.claudeAuthFailures)
	}
	if got := state.next[limits.ProviderClaude]; !got.Equal(recoveredAt.Add(liveLimitPollInterval)) {
		t.Fatalf("recovery next poll = %v, want %v", got, recoveredAt.Add(liveLimitPollInterval))
	}
	if got := countLogMessage(records, "limits.live_poll.claude_auth_recovered"); got != 1 {
		t.Fatalf("recovery log count = %d, want 1", got)
	}
}

func TestLiveLimitAuthBackoff_Bounded(t *testing.T) {
	if got := liveLimitAuthBackoff(1); got != liveLimitPollInterval {
		t.Fatalf("backoff(1) = %v, want %v", got, liveLimitPollInterval)
	}
	if got := liveLimitAuthBackoff(2); got != 2*liveLimitPollInterval {
		t.Fatalf("backoff(2) = %v, want %v", got, 2*liveLimitPollInterval)
	}
	if got := liveLimitAuthBackoff(6); got != liveLimitPollAuthBackoffMax {
		t.Fatalf("backoff(6) = %v, want max %v", got, liveLimitPollAuthBackoffMax)
	}
}

func countLogMessage(records []slog.Record, msg string) int {
	count := 0
	for _, record := range records {
		if record.Message == msg {
			count++
		}
	}
	return count
}
