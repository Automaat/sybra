package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestAgentRegistryRoundTripPreservesPersistedFields(t *testing.T) {
	startedAt := time.Date(2026, 6, 28, 20, 40, 0, 0, time.UTC)
	regDir := t.TempDir()
	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{SurviveRestartDir: regDir})

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
		sandboxHomeDir:           "/tmp/sandbox",
		convo:                    convoIO{stdinPath: "/tmp/stdin.fifo"},
		oneShot:                  true,
		requirePermissions:       true,
		sandboxMode:              "enforce",
		postResultWaitReason:     postResultWaitBackgroundTask,
		postResultWaitSince:      startedAt.Add(30 * time.Minute),
		forkSubagent:             true,
		detached:                 false,
		InputTokens:              1,
		OutputTokens:             2,
		CacheCreationInputTokens: 3,
		CacheReadInputTokens:     4,
		ReasoningTokens:          5,
		PremiumRequests:          6,
	}
	m.saveRegistry(context.Background(), original)

	records, err := m.reg.List()
	if err != nil {
		t.Fatalf("list records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	if wantProcStartedAt := processStartString(context.Background(), original.PID); wantProcStartedAt != "" && records[0].ProcStartedAt != wantProcStartedAt {
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
		SandboxHomeDir:     rehydrated.sandboxHomeDir,
		StartedAt:          rehydrated.StartedAt,
		StdinPath:          rehydrated.GetStdinPath(),
		OneShot:            rehydrated.oneShot,
		MaxTurns:           rehydrated.MaxTurns,
		RequirePermissions: rehydrated.requirePermissions,
		SandboxMode:        rehydrated.sandboxMode,
		ReasoningEffort:    rehydrated.ReasoningEffort,
		PostResultReason:   rehydrated.postResultWaitReason,
		PostResultSince:    rehydrated.postResultWaitSince,
		ForkSubagent:       rehydrated.forkSubagent,
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
		SandboxHomeDir:     original.sandboxHomeDir,
		StartedAt:          original.StartedAt,
		StdinPath:          original.GetStdinPath(),
		OneShot:            original.oneShot,
		MaxTurns:           original.MaxTurns,
		RequirePermissions: original.requirePermissions,
		SandboxMode:        original.sandboxMode,
		ReasoningEffort:    original.ReasoningEffort,
		PostResultReason:   original.postResultWaitReason,
		PostResultSince:    original.postResultWaitSince,
		ForkSubagent:       original.forkSubagent,
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

func TestRegistryStore_ConcurrentSaveDeleteList(t *testing.T) {
	regDir := t.TempDir()
	s, err := newRegistryStore(regDir)
	if err != nil {
		t.Fatalf("newRegistryStore: %v", err)
	}

	const workers = 16
	const iterations = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers*iterations*3)
	for worker := range workers {
		wg.Go(func() {
			for iteration := range iterations {
				id := fmt.Sprintf("agent-%02d-%02d", worker, iteration)
				fifo := agentFIFOPath(regDir, id)
				if mkErr := makeFIFO(fifo); mkErr != nil {
					errs <- fmt.Errorf("mkfifo %s: %w", id, mkErr)
					continue
				}
				if saveErr := s.Save(Record{
					ID:        id,
					Mode:      "interactive",
					Provider:  "claude",
					PID:       os.Getpid(),
					StartedAt: time.Now().UTC(),
					StdinPath: fifo,
				}); saveErr != nil {
					errs <- fmt.Errorf("save %s: %w", id, saveErr)
					continue
				}
				if _, listErr := s.List(); listErr != nil {
					errs <- fmt.Errorf("list %s: %w", id, listErr)
				}
				if deleteErr := s.Delete(id); deleteErr != nil {
					errs <- fmt.Errorf("delete %s: %w", id, deleteErr)
					continue
				}
				if _, statErr := os.Stat(fifo); !os.IsNotExist(statErr) {
					errs <- fmt.Errorf("fifo %s still present, stat err=%w", id, statErr)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	records, err := s.List()
	if err != nil {
		t.Fatalf("final list: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records left after concurrent delete = %d, want 0", len(records))
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
	SandboxHomeDir     string
	StartedAt          time.Time
	StdinPath          string
	OneShot            bool
	MaxTurns           int
	RequirePermissions bool
	SandboxMode        string
	ReasoningEffort    string
	PostResultReason   string
	PostResultSince    time.Time
	ForkSubagent       bool
}
