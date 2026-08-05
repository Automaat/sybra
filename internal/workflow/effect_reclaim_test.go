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
	engine := NewEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, agents, discardLogger())

	future := time.Now().UTC().Add(20 * time.Minute)
	tasks.Put(orphanedLeaseTask("t1", "workflow-engine-1-1", future, nil))

	if got := engine.ReclaimOrphanedEffectLeases(); got != 1 {
		t.Fatalf("ReclaimOrphanedEffectLeases() = %d, want 1", got)
	}
	if lease := leaseFor(t, tasks, "t1"); lease != nil {
		t.Fatalf("LeaseExpiresAt = %v, want nil so the live engine can claim", lease)
	}
}

// A survive-restart agent outlives the engine that spawned it. Releasing its
// step's claim would let a second agent start for work already in flight.
func TestReclaimOrphanedEffectLeases_SkipsTaskWithLiveAgent(t *testing.T) {
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, agents, discardLogger())

	future := time.Now().UTC().Add(20 * time.Minute)
	tasks.Put(orphanedLeaseTask("t1", "workflow-engine-1-1", future, nil))
	agents.running["t1"] = "reattached-agent"

	if got := engine.ReclaimOrphanedEffectLeases(); got != 0 {
		t.Fatalf("ReclaimOrphanedEffectLeases() = %d, want 0 while an agent is live", got)
	}
	if lease := leaseFor(t, tasks, "t1"); lease == nil {
		t.Fatal("LeaseExpiresAt = nil, want the reattached agent's claim left intact")
	}
}

func TestReclaimOrphanedEffectLeases_LeavesOwnAndCompletedClaims(t *testing.T) {
	tests := []struct {
		name      string
		owner     func(e *Engine) string
		completed bool
	}{
		{name: "own live claim", owner: func(e *Engine) string { return e.ownerID }},
		{name: "completed claim", owner: func(*Engine) string { return "workflow-engine-1-1" }, completed: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tasks := newMemTasks()
			engine := NewEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, newMockAgents(), discardLogger())

			future := time.Now().UTC().Add(20 * time.Minute)
			var completed *time.Time
			if tc.completed {
				done := time.Now().UTC()
				completed = &done
			}
			tasks.Put(orphanedLeaseTask("t1", tc.owner(engine), future, completed))

			if got := engine.ReclaimOrphanedEffectLeases(); got != 0 {
				t.Fatalf("ReclaimOrphanedEffectLeases() = %d, want 0", got)
			}
			if lease := leaseFor(t, tasks, "t1"); lease == nil {
				t.Fatal("LeaseExpiresAt = nil, want the claim left intact")
			}
		})
	}
}

// End-to-end: the fenced ClaimEffect that produced the stall must succeed once
// the orphan is reclaimed.
func TestReclaimOrphanedEffectLeases_UnfencesSubsequentClaim(t *testing.T) {
	tasks := newMemTasks()
	engine := NewEngine(newInlineTestStore(t, "noop", noopWorkflowYAML), tasks, newMockAgents(), discardLogger())

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
	}

	engine.ReclaimOrphanedEffectLeases()

	if _, err := tasks.ClaimWorkflowEffect("t1", claim); err != nil {
		t.Fatalf("ClaimWorkflowEffect after reclaim: %v, want success", err)
	}
}
