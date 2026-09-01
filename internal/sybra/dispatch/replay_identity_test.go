package dispatch

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/providerid"
)

func mutateIntent() agent.AttemptIntent {
	return agent.AttemptIntent{
		IntentID: "t1:simple-task-implement:g1:s1:implement:0",
		TaskID:   "t1", TaskGeneration: 7,
		Worktree: "/wt/t1", WorktreeGeneration: 7,
		Access: agent.AttemptAccessMutate, Role: agent.RoleImplementation,
		Provider: "claude", CapabilityCertified: true,
	}
}

func TestReplayIntentMatches_ToleratesADispatchDecision(t *testing.T) {
	tests := []struct {
		name   string
		change func(*agent.AttemptIntent)
	}{
		{"failed over to another provider", func(i *agent.AttemptIntent) { i.Provider = "codex" }},
		{"failed over again", func(i *agent.AttemptIntent) { i.Provider = "copilot" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a claimed attempt and a retry that re-resolved its dispatch
			stored := mutateIntent()
			replay := mutateIntent()
			tc.change(&replay)

			// When the replay is matched against the claim
			// Then it is still the same attempt
			if !replayIntentMatches(stored, replay) {
				t.Fatalf("replay refused after %s", tc.name)
			}
		})
	}
}

func TestReplayIntentMatches_RefusesADifferentAttempt(t *testing.T) {
	tests := []struct {
		name   string
		change func(*agent.AttemptIntent)
	}{
		{"another task", func(i *agent.AttemptIntent) { i.TaskID = "t2" }},
		{"another task generation", func(i *agent.AttemptIntent) { i.TaskGeneration = 8 }},
		{"another worktree", func(i *agent.AttemptIntent) { i.Worktree = "/wt/other" }},
		{"another worktree generation", func(i *agent.AttemptIntent) { i.WorktreeGeneration = 8 }},
		{"read access instead of write", func(i *agent.AttemptIntent) { i.Access = agent.AttemptAccessObserve }},
		{"another role", func(i *agent.AttemptIntent) { i.Role = agent.RoleReview }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a claimed attempt and a replay naming something else
			stored := mutateIntent()
			replay := mutateIntent()
			tc.change(&replay)

			// When the replay is matched against the claim
			// Then it is refused, because the claim covers a different action
			if replayIntentMatches(stored, replay) {
				t.Fatalf("replay accepted with %s", tc.name)
			}
		})
	}
}

func TestReplayIntentMatches_ExemptsOnlyTheVerifierWorktree(t *testing.T) {
	// Given a disposable verifier reading a fresh clone, and a writer that moved
	verifier := mutateIntent()
	verifier.Access, verifier.Role = agent.AttemptAccessObserve, agent.RoleTestRunner
	verifierReplay := verifier
	verifierReplay.Worktree = "/verification/runs/second/source"

	writer := mutateIntent()
	writerReplay := writer
	writerReplay.Worktree = "/wt/elsewhere"

	// When each replay is matched against its claim
	// Then only the verifier's fresh clone is exempt
	if !replayIntentMatches(verifier, verifierReplay) {
		t.Fatal("verifier replay refused for reading a fresh clone")
	}
	if replayIntentMatches(writer, writerReplay) {
		t.Fatal("a mutating attempt was allowed to move worktree")
	}
}

func TestAcquireReplayAfterFailoverKeepsTheClaimedProvider(t *testing.T) {
	// Given a live lease claimed for one provider, at a per-provider ceiling of one
	clock := &fakeClock{t: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}
	c := newTestController(t, clock, "epoch-1", Limits{ByProvider: map[string]int{
		providerid.Claude: 1, providerid.Codex: 1,
	}})
	wt := filepath.Join(t.TempDir(), "worktree")
	claimed := intent("intent-1", "task-1", wt, providerid.Claude, agent.AttemptAccessMutate)
	lease, err := c.Acquire(t.Context(), claimed)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := c.Bind(t.Context(), lease, agent.AttemptBinding{AgentID: "a1", PID: 4242}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// When the same effect is replayed after the gate resolved another provider
	failedOver := intent("intent-1", "task-1", wt, providerid.Codex, agent.AttemptAccessMutate)
	replayed, err := c.Acquire(t.Context(), failedOver)

	// Then the claim holds, and the running process still counts against the provider it started on
	if err != nil {
		t.Fatalf("replay after failover: %v", err)
	}
	if !replayed.Existing {
		t.Fatal("replay minted a second lease for a live claim")
	}
	other := intent("intent-2", "task-2", filepath.Join(t.TempDir(), "other"), providerid.Claude, agent.AttemptAccessMutate)
	if _, err := c.Acquire(t.Context(), other); err == nil {
		t.Fatal("a second claude attempt was admitted past the ceiling of one")
	}
}

func TestAcquireReplayStillRefusesADifferentAttempt(t *testing.T) {
	// Given a live lease
	clock := &fakeClock{t: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}
	c := newTestController(t, clock, "epoch-1", Limits{})
	wt := filepath.Join(t.TempDir(), "worktree")
	if _, err := c.Acquire(t.Context(), intent("intent-1", "task-1", wt, providerid.Claude, agent.AttemptAccessMutate)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// When its effect id is replayed for another task
	_, err := c.Acquire(t.Context(), intent("intent-1", "task-2", wt, providerid.Claude, agent.AttemptAccessMutate))

	// Then the claim is refused, since it covers a different action
	if !errors.Is(err, ErrIntentReplayMismatch) {
		t.Fatalf("err = %v, want ErrIntentReplayMismatch", err)
	}
}
