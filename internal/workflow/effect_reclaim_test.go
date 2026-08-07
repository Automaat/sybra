package workflow

import (
	"testing"
	"time"
)

const noopWorkflowYAML = `id: noop
name: Noop
trigger:
  on: task.created
steps:
  - id: plan
    name: Plan
    type: set_status
    config:
      status: planning
`

func orphanedLeaseTask(id, owner string, expiresAt time.Time, completed *time.Time) TaskInfo {
	return TaskInfo{
		ID:     id,
		Status: "planning",
		Workflow: &Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "plan",
			State:       ExecRunning,
			EffectLog: []EffectRecord{{
				ID:             EffectID{Generation: 17, StepSeq: 1, StepID: "plan", Pos: 0},
				IntentAt:       expiresAt.Add(-30 * time.Minute),
				Owner:          owner,
				LeaseExpiresAt: &expiresAt,
				CompletedAt:    completed,
			}},
		},
	}
}

func leaseFor(t *testing.T, tasks *memTasks, id string) *time.Time {
	t.Helper()
	got, err := tasks.GetTask(id)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if len(got.Workflow.EffectLog) != 1 {
		t.Fatalf("EffectLog = %+v, want exactly one record", got.Workflow.EffectLog)
	}
	return got.Workflow.EffectLog[0].LeaseExpiresAt
}

// A restart mints a fresh owner id, so the previous instance's still-valid
// lease fences the live engine out of its own step until the TTL lapses.
func TestReclaimOrphanedEffectLeases_ReleasesDeadOwnersClaim(t *testing.T) {
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, agents, discardLogger())

	future := time.Now().UTC().Add(20 * time.Minute)
	tasks.Put(orphanedLeaseTask("t1", "workflow-engine-1-1", future, nil))

	if got := engine.ReclaimOrphanedEffectLeases(); got != 1 {
		t.Fatalf("ReclaimOrphanedEffectLeases() = %d, want 1", got)
	}
	if lease := leaseFor(t, tasks, "t1"); lease != nil {
		t.Fatalf("LeaseExpiresAt = %v, want nil so the live engine can claim", lease)
		panic("unreachable")
	}
}

// A survive-restart agent outlives the engine that spawned it. Releasing its
// step's claim would let a second agent start for work already in flight.
func TestReclaimOrphanedEffectLeases_SkipsTaskWithLiveAgent(t *testing.T) {
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, agents, discardLogger())

	future := time.Now().UTC().Add(20 * time.Minute)
	tasks.Put(orphanedLeaseTask("t1", "workflow-engine-1-1", future, nil))
	agents.running["t1"] = "reattached-agent"

	if got := engine.ReclaimOrphanedEffectLeases(); got != 0 {
		t.Fatalf("ReclaimOrphanedEffectLeases() = %d, want 0 while an agent is live", got)
	}
	if lease := leaseFor(t, tasks, "t1"); lease == nil {
		t.Fatal("LeaseExpiresAt = nil, want the reattached agent's claim left intact")
		panic("unreachable")
	}
}

func TestReclaimOrphanedEffectLeases_LeavesClaimsItMustNotTouch(t *testing.T) {
	past := time.Now().UTC().Add(-20 * time.Minute)
	future := time.Now().UTC().Add(20 * time.Minute)

	tests := []struct {
		name    string
		owner   func(e *Engine) string
		expires time.Time
		arrange func(e *Engine, agents *mockAgents, tk *TaskInfo)
	}{
		{
			name:    "own live claim",
			owner:   func(e *Engine) string { return e.ownerID },
			expires: future,
		},
		{
			name:    "completed claim",
			owner:   func(*Engine) string { return "workflow-engine-1-1" },
			expires: future,
			arrange: func(_ *Engine, _ *mockAgents, tk *TaskInfo) {
				done := time.Now().UTC()
				tk.Workflow.EffectLog[0].CompletedAt = &done
			},
		},
		{
			// Intent-only: fences nobody, so rewriting only bumps generation.
			name:    "ownerless claim",
			owner:   func(*Engine) string { return "" },
			expires: future,
		},
		{
			// Already lapsed — ClaimEffect takes it over without help.
			name:    "expired lease",
			owner:   func(*Engine) string { return "workflow-engine-1-1" },
			expires: past,
		},
		{
			// Claimed before the agent registers, so HasRunningAgent misses it.
			name:    "task mid-dispatch",
			owner:   func(*Engine) string { return "workflow-engine-1-1" },
			expires: future,
			arrange: func(_ *Engine, agents *mockAgents, _ *TaskInfo) {
				agents.dispatchClaimed = map[string]bool{"t1": true}
			},
		},
		{
			name:    "terminal workflow",
			owner:   func(*Engine) string { return "workflow-engine-1-1" },
			expires: future,
			arrange: func(_ *Engine, _ *mockAgents, tk *TaskInfo) {
				tk.Workflow.State = ExecCompleted
			},
		},
		{
			// Homed on a follower: that owner is a live peer, not an orphan.
			name:    "task the dispatch gate rejects",
			owner:   func(*Engine) string { return "workflow-engine-1-1" },
			expires: future,
			arrange: func(e *Engine, _ *mockAgents, _ *TaskInfo) {
				e.SetDispatchGate(func(TaskInfo) bool { return false })
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tasks := newMemTasks()
			agents := newMockAgents()
			engine := NewTestEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, agents, discardLogger())

			tk := orphanedLeaseTask("t1", tc.owner(engine), tc.expires, nil)
			if tc.arrange != nil {
				tc.arrange(engine, agents, &tk)
			}
			tasks.Put(tk)

			if got := engine.ReclaimOrphanedEffectLeases(); got != 0 {
				t.Fatalf("ReclaimOrphanedEffectLeases() = %d, want 0", got)
			}
			if lease := leaseFor(t, tasks, "t1"); lease == nil {
				t.Fatal("LeaseExpiresAt = nil, want the claim left intact")
				panic("unreachable")
			}
		})
	}
}

// An instance that never dispatches must not rewrite the board's effect log.
func TestReclaimOrphanedEffectLeases_NoopWhenDispatchDisabled(t *testing.T) {
	tasks := newMemTasks()
	engine := NewTestEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, newMockAgents(), discardLogger())
	engine.SetAutoDispatch(false)

	tasks.Put(orphanedLeaseTask("t1", "workflow-engine-1-1", time.Now().UTC().Add(20*time.Minute), nil))

	if got := engine.ReclaimOrphanedEffectLeases(); got != 0 {
		t.Fatalf("ReclaimOrphanedEffectLeases() = %d, want 0 with dispatch disabled", got)
	}
	if lease := leaseFor(t, tasks, "t1"); lease == nil {
		t.Fatal("LeaseExpiresAt = nil, want the claim left intact")
		panic("unreachable")
	}
}

// End-to-end: the fenced ClaimEffect that produced the stall must succeed once
// the orphan is reclaimed.
func TestReclaimOrphanedEffectLeases_UnfencesSubsequentClaim(t *testing.T) {
	tasks := newMemTasks()
	engine := NewTestEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, newMockAgents(), discardLogger())

	future := time.Now().UTC().Add(20 * time.Minute)
	tasks.Put(orphanedLeaseTask("t1", "workflow-engine-1-1", future, nil))

	claim := EffectClaim{
		EffectID: EffectID{Generation: 17, StepSeq: 1, StepID: "plan", Pos: 0},
		Owner:    engine.ownerID,
		LeaseTTL: 30 * time.Minute,
		Now:      time.Now().UTC(),
	}
	if _, err := tasks.ClaimWorkflowEffect("t1", claim); err == nil {
		t.Fatal("ClaimWorkflowEffect succeeded before reclaim, want a fence conflict")
		panic("unreachable")
	}

	engine.ReclaimOrphanedEffectLeases()

	if _, err := tasks.ClaimWorkflowEffect("t1", claim); err != nil {
		t.Fatalf("ClaimWorkflowEffect after reclaim: %v, want success", err)
		panic("unreachable")
	}
}
