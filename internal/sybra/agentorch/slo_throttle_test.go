package agentorch

import (
	"errors"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evaluation"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestEffectiveMaxConcurrent(t *testing.T) {
	cases := []struct {
		name            string
		configured      int
		throttle        bool
		budgetExhausted bool
		want            int
	}{
		{"unlimited pool is never narrowed", 0, true, true, 0},
		{"throttle disabled returns configured", 4, false, true, 4},
		{"budget not exhausted returns configured", 4, true, false, 4},
		{"exhausted halves", 4, true, true, 2},
		{"exhausted floors at 1", 1, true, true, 1},
		{"odd configured rounds down then floors at 1", 3, true, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveMaxConcurrent(c.configured, c.throttle, c.budgetExhausted); got != c.want {
				t.Errorf("effectiveMaxConcurrent(%d,%v,%v) = %d, want %d",
					c.configured, c.throttle, c.budgetExhausted, got, c.want)
			}
		})
	}
}

// TestStartAgentWithAssignment_SLOThrottleNarrowsAdmissionOnly pins the
// default-off SLO concurrency throttle (#8650725d): once
// agent.evaluation.slo.throttle_on_budget_exhausted is enabled and the SLO
// error budget reads exhausted, the *workflow-driven* implementation
// dispatch path (StartAgentWithAssignment) is capped at
// effectiveMaxConcurrent even though the raw agent pool has room — but the
// manual/recovery entry point (StartAgent, used by App.StartAgent and
// recovery.RestartStaleInProgress) is never throttled.
func TestStartAgentWithAssignment_SLOThrottleNarrowsAdmissionOnly(t *testing.T) {
	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	am := newFakeClaudeManager(t, 4) // raw pool has plenty of room

	q, err := agentqueue.New(t.TempDir(), agentqueue.Options{}, discardSlogLogger())
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}

	noPermissions := false
	o := New(tm, nil, am, nil, discardSlogLogger(), nil, &config.Config{
		Agent: config.AgentDefaults{
			ResearchMachineDir: t.TempDir(),
			RequirePermissions: &noPermissions,
			MaxConcurrent:      4,
		},
		Evaluation: config.EvaluationConfig{
			SLO: config.SLOTargets{ThrottleOnBudgetExhausted: true},
		},
	})
	o.SetQueue(q)
	o.SetSLOReport(evaluation.SLOReport{ErrorBudgetRemaining: 0})

	first := newAgentTask(t, tm, "first")
	if _, _, err := o.StartAgentWithAssignment(first.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{}); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	t.Cleanup(func() { am.KillAgentsForTask(first.ID, 5*time.Second) })

	second := newAgentTask(t, tm, "second")
	if _, _, err := o.StartAgentWithAssignment(second.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{}); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	t.Cleanup(func() { am.KillAgentsForTask(second.ID, 5*time.Second) })

	// effectiveMaxConcurrent(4, true, true) == 2: a third workflow dispatch
	// must be throttled into the queue even though the raw pool (cap 4) has
	// two more slots free.
	third := newAgentTask(t, tm, "third throttled by SLO budget")
	_, _, err = o.StartAgentWithAssignment(third.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{})
	if !errors.Is(err, workflow.ErrAgentPoolBusy) {
		t.Fatalf("third dispatch err = %v, want wrapping workflow.ErrAgentPoolBusy (SLO-throttled)", err)
	}
	if got := am.RunningCount(); got != 2 {
		t.Fatalf("RunningCount after throttled third = %d, want 2 (raw pool must not have granted a 3rd slot)", got)
	}

	// Recovery/manual dispatch (StartAgent, no admission gate) must remain
	// exempt from the throttle and claim the raw pool's still-free capacity.
	fourth := newAgentTask(t, tm, "fourth via manual/recovery entry point")
	ag, err := o.StartAgent(fourth.ID, "headless", "go", true, false)
	if err != nil {
		t.Fatalf("StartAgent(fourth) unexpected err: %v", err)
	}
	if ag == nil || ag.State == agent.StateQueued {
		t.Fatalf("StartAgent(fourth) = %+v, want a live agent (recovery/manual exempt from SLO throttle)", ag)
	}
	t.Cleanup(func() { am.KillAgentsForTask(fourth.ID, 5*time.Second) })
}

// TestStartAgentWithAssignment_SLOThrottleDisabledByDefault proves the
// throttle is fully inert unless explicitly enabled: an exhausted SLO report
// with ThrottleOnBudgetExhausted left at its shipped default (false) must
// never narrow admission.
func TestStartAgentWithAssignment_SLOThrottleDisabledByDefault(t *testing.T) {
	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	am := newFakeClaudeManager(t, 4)

	noPermissions := false
	o := New(tm, nil, am, nil, discardSlogLogger(), nil, &config.Config{
		Agent: config.AgentDefaults{
			ResearchMachineDir: t.TempDir(),
			RequirePermissions: &noPermissions,
			MaxConcurrent:      4,
		},
		// SLO.ThrottleOnBudgetExhausted left zero-value (false).
	})
	o.SetSLOReport(evaluation.SLOReport{ErrorBudgetRemaining: 0})

	for i, title := range []string{"a", "b", "c"} {
		tk := newAgentTask(t, tm, title)
		if _, _, err := o.StartAgentWithAssignment(tk.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{}); err != nil {
			t.Fatalf("dispatch %d: unexpected err (throttle must be a no-op when disabled): %v", i, err)
		}
		t.Cleanup(func() { am.KillAgentsForTask(tk.ID, 5*time.Second) })
	}
	if got := am.RunningCount(); got != 3 {
		t.Fatalf("RunningCount = %d, want 3 (throttle disabled must not narrow admission)", got)
	}
}
