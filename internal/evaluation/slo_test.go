package evaluation

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func TestEvaluateSLOs(t *testing.T) {
	targets := SLOTargets{
		MinAutonomyRate:      0.80,
		MinCIFirstPassRate:   0.80,
		MaxReworkRate:        0.40,
		MaxIdenticalRetryCap: 3,
		MaxRestartsPerHour:   1,
	}

	cases := []struct {
		name          string
		sc            Scorecard
		sig           SLOSignals
		wantCompliant bool
		wantBreaches  []string
	}{
		{
			name:          "all compliant",
			sc:            Scorecard{TasksLanded: 10, AutonomyRate: 0.9, CIFirstPassRate: 0.85, ReworkTasks: 1},
			sig:           SLOSignals{IdenticalRetryMax: 2, RestartsPerHour: 0.5},
			wantCompliant: true,
		},
		{
			name:          "on target is compliant (>=, <=)",
			sc:            Scorecard{TasksLanded: 10, AutonomyRate: 0.80, CIFirstPassRate: 0.80, ReworkTasks: 4},
			sig:           SLOSignals{IdenticalRetryMax: 3, RestartsPerHour: 1},
			wantCompliant: true,
		},
		{
			name:          "autonomy breach",
			sc:            Scorecard{TasksLanded: 10, AutonomyRate: 0.5, CIFirstPassRate: 0.9, ReworkTasks: 0},
			sig:           SLOSignals{IdenticalRetryMax: 0, RestartsPerHour: 0},
			wantCompliant: false,
			wantBreaches:  []string{sloNameAutonomy},
		},
		{
			name:          "ci_first_pass breach",
			sc:            Scorecard{TasksLanded: 10, AutonomyRate: 0.9, CIFirstPassRate: 0.5, ReworkTasks: 0},
			sig:           SLOSignals{},
			wantCompliant: false,
			wantBreaches:  []string{sloNameCIFirstPass},
		},
		{
			name:          "rework breach",
			sc:            Scorecard{TasksLanded: 10, AutonomyRate: 0.9, CIFirstPassRate: 0.9, ReworkTasks: 5},
			sig:           SLOSignals{},
			wantCompliant: false,
			wantBreaches:  []string{sloNameRework},
		},
		{
			name:          "identical retry cap breach",
			sc:            Scorecard{TasksLanded: 10, AutonomyRate: 0.9, CIFirstPassRate: 0.9},
			sig:           SLOSignals{IdenticalRetryMax: 4},
			wantCompliant: false,
			wantBreaches:  []string{sloNameIdenticalRetry},
		},
		{
			name:          "restart cadence breach",
			sc:            Scorecard{TasksLanded: 10, AutonomyRate: 0.9, CIFirstPassRate: 0.9},
			sig:           SLOSignals{RestartsPerHour: 2.5},
			wantCompliant: false,
			wantBreaches:  []string{sloNameRestartCadence},
		},
		{
			name:          "multiple breaches",
			sc:            Scorecard{TasksLanded: 10, AutonomyRate: 0.1, CIFirstPassRate: 0.1, ReworkTasks: 9},
			sig:           SLOSignals{IdenticalRetryMax: 10, RestartsPerHour: 10},
			wantCompliant: false,
			wantBreaches:  []string{sloNameAutonomy, sloNameCIFirstPass, sloNameRework, sloNameIdenticalRetry, sloNameRestartCadence},
		},
		{
			name:          "empty window is compliant, not a breach",
			sc:            Scorecard{},
			sig:           SLOSignals{},
			wantCompliant: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EvaluateSLOs(c.sc, c.sig, targets)
			if got.Compliant != c.wantCompliant {
				t.Errorf("Compliant = %v, want %v (breaches=%v)", got.Compliant, c.wantCompliant, got.Breaches)
			}
			if len(got.Breaches) != len(c.wantBreaches) {
				t.Fatalf("Breaches = %v, want %v", got.Breaches, c.wantBreaches)
			}
			for i, b := range c.wantBreaches {
				if got.Breaches[i] != b {
					t.Errorf("Breaches[%d] = %q, want %q", i, got.Breaches[i], b)
				}
			}
			if len(got.Statuses) != 5 {
				t.Fatalf("Statuses = %d entries, want 5", len(got.Statuses))
			}
		})
	}
}

// TestEvaluateSLOs_ErrorBudget proves ErrorBudgetRemaining is the minimum
// across all five statuses (a fleet is only as healthy as its worst SLO) and
// that it reaches exactly 0 the moment any single SLO is on the breach edge.
func TestEvaluateSLOs_ErrorBudget(t *testing.T) {
	targets := DefaultSLOTargets()

	healthy := EvaluateSLOs(Scorecard{TasksLanded: 10, AutonomyRate: 1.0, CIFirstPassRate: 1.0, ReworkTasks: 0}, SLOSignals{}, targets)
	if healthy.ErrorBudgetRemaining != 1 {
		t.Errorf("perfect scorecard ErrorBudgetRemaining = %v, want 1", healthy.ErrorBudgetRemaining)
	}

	onEdge := EvaluateSLOs(Scorecard{TasksLanded: 10, AutonomyRate: targets.MinAutonomyRate, CIFirstPassRate: 1.0, ReworkTasks: 0}, SLOSignals{}, targets)
	if onEdge.ErrorBudgetRemaining != 0 {
		t.Errorf("on-target autonomy ErrorBudgetRemaining = %v, want 0 (no headroom left)", onEdge.ErrorBudgetRemaining)
	}
	if !onEdge.Compliant {
		t.Errorf("on-target autonomy should still be Met/Compliant (>=, not >)")
	}

	breached := EvaluateSLOs(Scorecard{TasksLanded: 10, AutonomyRate: 0, CIFirstPassRate: 1.0, ReworkTasks: 0}, SLOSignals{}, targets)
	if breached.ErrorBudgetRemaining != 0 {
		t.Errorf("breached autonomy ErrorBudgetRemaining = %v, want 0", breached.ErrorBudgetRemaining)
	}
	if breached.Compliant {
		t.Error("breached autonomy should not be Compliant")
	}
}

func TestScanIdenticalRetryCap(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	failedRun := func(taskID, agentID string, ts time.Time) audit.Event {
		return audit.Event{Type: audit.EventAgentFailed, TaskID: taskID, AgentID: agentID, Timestamp: ts}
	}
	events := []audit.Event{
		failedRun("A", "a1", since.Add(time.Minute)),
		failedRun("A", "a2", since.Add(2*time.Minute)),
		failedRun("A", "a3", since.Add(3*time.Minute)),
		failedRun("B", "b1", since.Add(time.Minute)),
		failedRun("A", "a4-outside", until.Add(time.Minute)), // outside window
	}
	if got := scanIdenticalRetryCap(events, since, until); got != 3 {
		t.Fatalf("scanIdenticalRetryCap = %d, want 3 (task A's in-window failures)", got)
	}
}

func TestScanRestartCadence(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	statusChange := func(taskID, from, to string, ts time.Time) audit.Event {
		return audit.Event{Type: audit.EventTaskStatusChanged, TaskID: taskID, Timestamp: ts,
			Data: map[string]any{"from": from, "to": to}}
	}
	dispatched := func(taskID string, ts time.Time) audit.Event {
		return audit.Event{Type: audit.EventTaskDispatched, TaskID: taskID, Timestamp: ts}
	}

	events := []audit.Event{
		// Automatic: monitor auto-retry, no task.dispatched nearby.
		statusChange("A", "human-required", "in-progress", since.Add(5*time.Minute)),
		// Automatic: PR-monitor blocker reconciliation.
		statusChange("B", "human-required", "in-review", since.Add(10*time.Minute)),
		// Manual: GUI dispatch logs task.dispatched at (essentially) the same time.
		statusChange("C", "human-required", "in-progress", since.Add(15*time.Minute)),
		dispatched("C", since.Add(15*time.Minute).Add(2*time.Second)),
		// Not a human-required exit at all — must not count.
		statusChange("D", "todo", "in-progress", since.Add(20*time.Minute)),
	}
	got := scanRestartCadence(events, since, until)
	if want := 2.0; got != want {
		t.Fatalf("scanRestartCadence = %v restarts/hr, want %v (A and B automatic, C manual, D irrelevant)", got, want)
	}
}

// TestSLO_GoldenFixtures proves each #2441 incident class has a
// failing-before / clean-passing-after pair: the SLO gate must flag the
// regression and clear once the fleet recovers. Fixtures are built from
// audit.Event + stats.RunRecord (not literal JSON testdata) so the whole
// pipeline (Compute + ComputeSLOSignals + EvaluateSLOs) is exercised
// end-to-end, deterministically and offline — no fake clock, no network,
// fully reproducible in CI (see scripts/check-autonomy-slo.sh).
func TestSLO_GoldenFixtures(t *testing.T) {
	targets := DefaultSLOTargets()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)

	landed := func(taskID string, ts time.Time, outcome string) audit.Event {
		return audit.Event{Type: audit.EventTaskLanded, TaskID: taskID, Timestamp: ts, Data: map[string]any{"outcome": outcome}}
	}
	humanRequired := func(taskID string, ts time.Time) audit.Event {
		return audit.Event{Type: audit.EventTaskStatusChanged, TaskID: taskID, Timestamp: ts,
			Data: map[string]any{"from": "in-review", "to": "human-required"}}
	}
	autoRestart := func(taskID string, ts time.Time) audit.Event {
		return audit.Event{Type: audit.EventTaskStatusChanged, TaskID: taskID, Timestamp: ts,
			Data: map[string]any{"from": "human-required", "to": "in-progress"}}
	}
	ciFix := func(taskID string, ts time.Time) audit.Event {
		return audit.Event{Type: audit.EventPRCIFailureDetected, TaskID: taskID, Timestamp: ts}
	}
	rework := func(taskID string, ts time.Time) []audit.Event {
		return []audit.Event{
			{Type: audit.EventTaskStatusChanged, TaskID: taskID, Timestamp: ts, Data: map[string]any{"from": "todo", "to": "in-progress"}},
			{Type: audit.EventTaskStatusChanged, TaskID: taskID, Timestamp: ts.Add(time.Minute), Data: map[string]any{"from": "todo", "to": "in-progress"}},
		}
	}
	failedRun := func(taskID, agentID string, ts time.Time) audit.Event {
		return audit.Event{Type: audit.EventAgentFailed, TaskID: taskID, AgentID: agentID, Timestamp: ts}
	}

	type fixture struct {
		// class names an incident class from #2441.
		class string
		// dim is the SLOStatus.Name this class is expected to move.
		dim          string
		beforeEvents []audit.Event
		afterEvents  []audit.Event
	}

	fixtures := []fixture{
		{
			// failover: provider/model failover that never converges leaves
			// the task escalated to a human, denting autonomy. After the fix,
			// the same task lands without a human touch.
			class: "failover",
			dim:   sloNameAutonomy,
			beforeEvents: []audit.Event{
				landed("failover-1", since.Add(5*time.Minute), "merged"),
				humanRequired("failover-1", since.Add(time.Minute)),
			},
			afterEvents: []audit.Event{
				landed("failover-2", since.Add(5*time.Minute), "merged"),
			},
		},
		{
			// retry-storm: a zero-signal loop burns through failed runs on one
			// task well past the identical-retry cap.
			class: "retry-storm",
			dim:   sloNameIdenticalRetry,
			beforeEvents: []audit.Event{
				failedRun("storm-1", "a1", since.Add(time.Minute)),
				failedRun("storm-1", "a2", since.Add(2*time.Minute)),
				failedRun("storm-1", "a3", since.Add(3*time.Minute)),
				failedRun("storm-1", "a4", since.Add(4*time.Minute)),
			},
			afterEvents: []audit.Event{
				failedRun("storm-2", "b1", since.Add(time.Minute)),
				failedRun("storm-2", "b2", since.Add(2*time.Minute)),
			},
		},
		{
			// malformed-verdict: a review/eval agent emits output the engine
			// can't parse, bouncing the task's status back and forth (rework)
			// before it converges.
			class:        "malformed-verdict",
			dim:          sloNameRework,
			beforeEvents: append(rework("verdict-1", since.Add(time.Minute)), landed("verdict-1", since.Add(10*time.Minute), "merged")),
			afterEvents: []audit.Event{
				landed("verdict-2", since.Add(10*time.Minute), "merged"),
			},
		},
		{
			// worktree-loss: a corrupted/missing worktree forces the monitor to
			// auto-restart the same task repeatedly inside the hour.
			class: "worktree-loss",
			dim:   sloNameRestartCadence,
			beforeEvents: []audit.Event{
				autoRestart("wt-1", since.Add(5*time.Minute)),
				autoRestart("wt-1", since.Add(20*time.Minute)),
			},
			afterEvents: []audit.Event{
				autoRestart("wt-2", since.Add(5*time.Minute)),
			},
		},
		{
			// ci-flake: a flaky check forces a CI-fix agent on most landings,
			// tanking the CI-first-pass rate.
			class: "ci-flake",
			dim:   sloNameCIFirstPass,
			beforeEvents: []audit.Event{
				landed("flake-1", since.Add(10*time.Minute), "merged"),
				ciFix("flake-1", since.Add(time.Minute)),
			},
			afterEvents: []audit.Event{
				landed("flake-2", since.Add(10*time.Minute), "merged"),
			},
		},
		{
			// auth-loss: a provider auth outage forces the same repeated
			// human-required -> in-progress auto-recovery restart worktree-loss
			// exercises, via a different root cause (see auth-loss vs
			// worktree-loss: both converge on the restart_cadence SLO by
			// design — cadence is a shared reliability signal, not
			// root-cause-specific).
			class: "auth-loss",
			dim:   sloNameRestartCadence,
			beforeEvents: []audit.Event{
				autoRestart("auth-1", since.Add(2*time.Minute)),
				autoRestart("auth-1", since.Add(35*time.Minute)),
			},
			afterEvents: []audit.Event{
				autoRestart("auth-2", since.Add(2*time.Minute)),
			},
		},
		{
			// shutdown-at-mutation: the server restarts mid-write; the
			// interrupted run is retried repeatedly past the identical-retry
			// cap once reattachment can't resolve the torn state cleanly.
			class: "shutdown-at-mutation",
			dim:   sloNameIdenticalRetry,
			beforeEvents: []audit.Event{
				failedRun("shutdown-1", "c1", since.Add(time.Minute)),
				failedRun("shutdown-1", "c2", since.Add(2*time.Minute)),
				failedRun("shutdown-1", "c3", since.Add(3*time.Minute)),
				failedRun("shutdown-1", "c4", since.Add(4*time.Minute)),
			},
			afterEvents: []audit.Event{
				failedRun("shutdown-2", "d1", since.Add(time.Minute)),
			},
		},
		{
			// reattach: a botched reattach after restart re-parks the task
			// human-required and the monitor keeps auto-restarting it.
			class: "reattach",
			dim:   sloNameRestartCadence,
			beforeEvents: []audit.Event{
				autoRestart("reattach-1", since.Add(3*time.Minute)),
				autoRestart("reattach-1", since.Add(40*time.Minute)),
			},
			afterEvents: []audit.Event{
				autoRestart("reattach-2", since.Add(3*time.Minute)),
			},
		},
	}

	for _, f := range fixtures {
		t.Run(f.class, func(t *testing.T) {
			before := EvaluateSLOs(Compute(nil, f.beforeEvents, since, until), ComputeSLOSignals(f.beforeEvents, since, until), targets)
			if before.Compliant {
				t.Fatalf("%s: before-fixture reads Compliant, want a breach on %q\nreport: %+v", f.class, f.dim, before)
			}
			if !containsBreach(before.Breaches, f.dim) {
				t.Fatalf("%s: before-fixture breaches = %v, want %q among them", f.class, before.Breaches, f.dim)
			}

			after := EvaluateSLOs(Compute(nil, f.afterEvents, since, until), ComputeSLOSignals(f.afterEvents, since, until), targets)
			if !after.Compliant {
				t.Fatalf("%s: after-fixture reads non-compliant, want clean\nreport: %+v", f.class, after)
			}

			t.Logf("slo: %-24s before=breach(%s) after=compliant budget=%.2f", f.class, f.dim, after.ErrorBudgetRemaining)
		})
	}
}

func containsBreach(breaches []string, name string) bool {
	return slices.Contains(breaches, name)
}

func TestDefaultSLOTargets(t *testing.T) {
	got := DefaultSLOTargets()
	want := fmt.Sprintf("%+v", SLOTargets{
		MinAutonomyRate: 0.80, MinCIFirstPassRate: 0.80, MaxReworkRate: 0.40,
		MaxIdenticalRetryCap: 3, MaxRestartsPerHour: 1,
	})
	if got := fmt.Sprintf("%+v", got); got != want {
		t.Errorf("DefaultSLOTargets() = %s, want %s", got, want)
	}
}
