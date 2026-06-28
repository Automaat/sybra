package agent

import (
	"context"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestAgentRegistryRoundTripPreservesPersistedFields(t *testing.T) {
	startedAt := time.Date(2026, 6, 28, 20, 40, 0, 0, time.UTC)
	m := NewManager(context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	if err := m.EnableSurviveRestart(t.TempDir()); err != nil {
		t.Fatalf("EnableSurviveRestart: %v", err)
	}

	original := &Agent{
		ID:                       "agent-1",
		TaskID:                   "task-1",
		Name:                     "implementation",
		Mode:                     "interactive",
		State:                    StatePaused,
		SessionID:                "session-1",
		CostUSD:                  99,
		StartedAt:                startedAt,
		LastEventAt:              startedAt.Add(time.Hour),
		LogPath:                  "/tmp/sybra-agent.ndjson",
		PID:                      os.Getpid(),
		Provider:                 "codex",
		Model:                    "gpt-5",
		ExperimentID:             "experiment-1",
		VariantID:                "variant-1",
		AssignmentUnit:           "task",
		AssignmentKey:            "task-1",
		ReasoningEffort:          "high",
		MaxTurns:                 12,
		sessionCWD:               "/tmp/worktree",
		convo:                    convoIO{stdinPath: "/tmp/stdin.fifo"},
		oneShot:                  true,
		requirePermissions:       true,
		detached:                 false,
		InputTokens:              1,
		OutputTokens:             2,
		CacheCreationInputTokens: 3,
		CacheReadInputTokens:     4,
		ReasoningTokens:          5,
		PremiumRequests:          6,
	}
	m.saveRegistry(original)

	records, err := m.reg.List()
	if err != nil {
		t.Fatalf("list records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	if wantProcStartedAt := processStartString(original.PID); wantProcStartedAt != "" && records[0].ProcStartedAt != wantProcStartedAt {
		t.Fatalf("ProcStartedAt = %q, want %q", records[0].ProcStartedAt, wantProcStartedAt)
	}

	beforeRehydrate := time.Now().UTC()
	rehydrated := fromRecord(records[0])
	afterRehydrate := time.Now().UTC()

	got := persistedAgentFields{
		ID:                 rehydrated.ID,
		TaskID:             rehydrated.TaskID,
		Name:               rehydrated.Name,
		Mode:               rehydrated.Mode,
		Provider:           rehydrated.Provider,
		Model:              rehydrated.Model,
		ExperimentID:       rehydrated.ExperimentID,
		VariantID:          rehydrated.VariantID,
		AssignmentUnit:     rehydrated.AssignmentUnit,
		AssignmentKey:      rehydrated.AssignmentKey,
		PID:                rehydrated.PID,
		SessionID:          rehydrated.SessionID,
		LogPath:            rehydrated.LogPath,
		SessionCWD:         rehydrated.sessionCWD,
		StartedAt:          rehydrated.StartedAt,
		StdinPath:          rehydrated.GetStdinPath(),
		OneShot:            rehydrated.oneShot,
		MaxTurns:           rehydrated.MaxTurns,
		RequirePermissions: rehydrated.requirePermissions,
		ReasoningEffort:    rehydrated.ReasoningEffort,
	}
	want := persistedAgentFields{
		ID:                 original.ID,
		TaskID:             original.TaskID,
		Name:               original.Name,
		Mode:               original.Mode,
		Provider:           original.Provider,
		Model:              original.Model,
		ExperimentID:       original.ExperimentID,
		VariantID:          original.VariantID,
		AssignmentUnit:     original.AssignmentUnit,
		AssignmentKey:      original.AssignmentKey,
		PID:                original.PID,
		SessionID:          original.SessionID,
		LogPath:            original.LogPath,
		SessionCWD:         original.sessionCWD,
		StartedAt:          original.StartedAt,
		StdinPath:          original.GetStdinPath(),
		OneShot:            original.oneShot,
		MaxTurns:           original.MaxTurns,
		RequirePermissions: original.requirePermissions,
		ReasoningEffort:    original.ReasoningEffort,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted fields mismatch\ngot:  %#v\nwant: %#v", got, want)
	}

	if rehydrated.State != StateRunning {
		t.Fatalf("State = %q, want %q", rehydrated.State, StateRunning)
	}
	if rehydrated.LastEventAt.Before(beforeRehydrate) || rehydrated.LastEventAt.After(afterRehydrate) {
		t.Fatalf("LastEventAt = %s, want between %s and %s", rehydrated.LastEventAt, beforeRehydrate, afterRehydrate)
	}
	if !rehydrated.detached {
		t.Fatal("detached = false, want true")
	}
	if rehydrated.CostUSD != 0 ||
		rehydrated.InputTokens != 0 ||
		rehydrated.OutputTokens != 0 ||
		rehydrated.CacheCreationInputTokens != 0 ||
		rehydrated.CacheReadInputTokens != 0 ||
		rehydrated.ReasoningTokens != 0 ||
		rehydrated.PremiumRequests != 0 {
		t.Fatalf("usage fields rehydrated from registry, want zero: %#v", rehydrated)
	}
}

type persistedAgentFields struct {
	ID                 string
	TaskID             string
	Name               string
	Mode               string
	Provider           string
	Model              string
	ExperimentID       string
	VariantID          string
	AssignmentUnit     string
	AssignmentKey      string
	PID                int
	SessionID          string
	LogPath            string
	SessionCWD         string
	StartedAt          time.Time
	StdinPath          string
	OneShot            bool
	MaxTurns           int
	RequirePermissions bool
	ReasoningEffort    string
}
