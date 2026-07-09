package workflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

func TestClassifyAgentStartError(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantPermanent bool
		wantContains  string
	}{
		{
			name: "nil yields empty",
			err:  nil,
		},
		{
			name:          "project not registered is permanent",
			err:           fmt.Errorf("worktree required: %w", project.ErrProjectNotRegistered),
			wantPermanent: true,
			wantContains:  "project not registered",
		},
		{
			name:         "generic error is transient",
			err:          errors.New("fetch origin: connection refused"),
			wantContains: "agent start failed: fetch origin: connection refused",
		},
		{
			name:          "rebase failed is permanent",
			err:           fmt.Errorf("prepare worktree: %w", worktreeerr.ErrRebaseFailed),
			wantPermanent: true,
			wantContains:  "branch stale: rebase failed before agent start",
		},
		{
			name:         "transient fetch failure is not permanent",
			err:          fmt.Errorf("prepare worktree: %w", worktreeerr.ErrTransientFetch),
			wantContains: "agent start delayed: transient network failure",
		},
		{
			name:         "provider unhealthy is transient",
			err:          &provider.UnhealthyError{Provider: "codex", Reason: "rate_limited"},
			wantContains: "agent start blocked: provider codex unhealthy (rate_limited)",
		},
		{
			name:         "long error gets truncated",
			err:          errors.New(strings.Repeat("x", startReasonMaxLen*2)),
			wantContains: "...",
		},
		{
			name:          "no project assigned is permanent",
			err:           fmt.Errorf("task t1 has no project_id: refusing to start plan agent without isolated worktree: %w", ErrNoProjectAssigned),
			wantPermanent: true,
			wantContains:  "no project could be assigned",
		},
		{
			name:          "task cost exceeded is permanent",
			err:           fmt.Errorf("start agent: %w: $25.00 spent across 5 run(s), limit $25.00", ErrTaskCostExceeded),
			wantPermanent: true,
			wantContains:  "task cumulative cost exceeds agent.max_task_cost_usd",
		},
		{
			name: "agent pool busy yields empty and transient",
			err:  fmt.Errorf("start agent: %w", ErrAgentPoolBusy),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, permanent := ClassifyAgentStartError(tc.err)
			if tc.err == nil {
				if reason != "" || permanent {
					t.Fatalf("nil err: got reason=%q permanent=%v, want empty/false", reason, permanent)
				}
				return
			}
			if permanent != tc.wantPermanent {
				t.Errorf("permanent: got %v, want %v", permanent, tc.wantPermanent)
			}
			if !strings.Contains(reason, tc.wantContains) {
				t.Errorf("reason %q missing %q", reason, tc.wantContains)
			}
			if len(reason) > startReasonMaxLen {
				t.Errorf("reason length %d exceeds cap %d", len(reason), startReasonMaxLen)
			}
		})
	}
}

func TestClassifyAgentStartError_DispatchInFlightSuppressed(t *testing.T) {
	// A dispatch-in-flight outcome is benign: the holder of the claim produces
	// the agent. It must yield an empty reason (no status_reason written) and
	// be non-permanent so the resume loop leaves the task alone.
	reason, permanent := ClassifyAgentStartError(ErrDispatchInFlight)
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
	if permanent {
		t.Error("dispatch-in-flight must not be permanent")
	}
	// Also when wrapped.
	wrapped := fmt.Errorf("start agent: %w", ErrDispatchInFlight)
	if r, p := ClassifyAgentStartError(wrapped); r != "" || p {
		t.Errorf("wrapped: got reason=%q permanent=%v, want empty/false", r, p)
	}
}

func TestClassifyAgentStartError_AgentPoolBusySuppressed(t *testing.T) {
	wrapped := fmt.Errorf("start agent: %w", ErrAgentPoolBusy)
	reason, permanent := ClassifyAgentStartError(wrapped)
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
	if permanent {
		t.Error("agent-pool-busy must not be permanent")
	}
	if !transientAgentStartError(wrapped) {
		t.Error("transientAgentStartError() = false, want true")
	}
}

func TestClassifyAgentStartError_AgentRunningSuppressed(t *testing.T) {
	// PrepareForTask refusing to reuse a worktree a tracked agent is still
	// live in (sybra#1495) is benign and self-healing — the agent's own
	// completion drives the workflow forward. Must yield an empty reason and
	// be non-permanent, same as ErrDispatchInFlight.
	wrapped := fmt.Errorf("prepare worktree for reuse: %w", worktreeerr.ErrAgentRunning)
	reason, permanent := ClassifyAgentStartError(wrapped)
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
	if permanent {
		t.Error("agent-running must not be permanent")
	}
	if !transientAgentStartError(wrapped) {
		t.Error("transientAgentStartError() = false, want true")
	}
}

func TestSurfaceStartFailure_DispatchInFlightIsNoOp(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	engine.surfaceStartFailure("t1", "in-progress", ErrDispatchInFlight, nil, "")

	got, _ := tasks.GetTask("t1")
	if got.Status != "in-progress" {
		t.Errorf("dispatch-in-flight flipped status: got %q, want in-progress", got.Status)
	}
	if reason := tasks.Reason("t1"); reason != "" {
		t.Errorf("dispatch-in-flight wrote reason %q, want empty", reason)
	}
}

func TestSurfaceStartFailure_TransientKeepsStatus(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	engine.surfaceStartFailure("t1", "in-progress", errors.New("git fetch: timeout"), nil, "")

	got, _ := tasks.GetTask("t1")
	if got.Status != "in-progress" {
		t.Errorf("transient failure flipped status: got %q, want in-progress", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "git fetch: timeout") {
		t.Errorf("reason %q missing transient error text", reason)
	}
}

func TestSurfaceStartFailure_TransientFetchKeepsStatus(t *testing.T) {
	// Regression guard for the bug this PR fixes: a network blip during
	// worktree reconcile must never park the task human-required — it should
	// behave exactly like any other transient failure and let the resume loop
	// retry once connectivity recovers.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	wrapped := fmt.Errorf("prepare worktree: %w", worktreeerr.ErrTransientFetch)
	engine.surfaceStartFailure("t1", "in-progress", wrapped, nil, "")

	got, _ := tasks.GetTask("t1")
	if got.Status != "in-progress" {
		t.Errorf("transient fetch failure flipped status: got %q, want in-progress", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "transient network failure") {
		t.Errorf("reason %q missing transient-fetch classification", reason)
	}
}

func TestIsTransientFetchReason(t *testing.T) {
	t.Parallel()
	if !isTransientFetchReason(transientFetchStatusReason) {
		t.Fatal("expected canonical transient fetch reason to match")
	}
	if isTransientFetchReason("agent start failed: transient network failure reconciling worktree with remote") {
		t.Fatal("unexpected match for non-canonical reason")
	}
}

func TestSurfaceStartFailure_PermanentFlipsToHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	wrapped := fmt.Errorf("worktree required: %w", project.ErrProjectNotRegistered)
	engine.surfaceStartFailure("t1", "in-progress", wrapped, nil, "")

	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("permanent failure should flip to human-required, got %q", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "project not registered") {
		t.Errorf("reason %q missing permanent classification", reason)
	}
}

func TestSurfaceStartFailure_RebaseFailedFlipsToHumanRequired(t *testing.T) {
	// Engine.surfaceStartFailure must classify ErrRebaseFailed as permanent
	// too, as defense in depth: the three Engine-routed callers already flip
	// to human-required via markRebaseBlocked before the error reaches here,
	// but should that upstream guard ever regress, this classification is
	// what stops the resume loop from hammering the doomed rebase forever.
	// The actual regression this PR fixes is in the recovery.StartPRFixAgent
	// path, which skips markRebaseBlocked entirely — see
	// TestRestartStalePRFixRebaseFailedFlipsToHumanRequired in
	// internal/recovery/recovery_test.go.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	wrapped := fmt.Errorf("prepare worktree: %w", worktreeerr.ErrRebaseFailed)
	engine.surfaceStartFailure("t1", "in-progress", wrapped, nil, "")

	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("rebase failure should flip to human-required, got %q", got.Status)
	}
	reason := tasks.Reason("t1")
	if !strings.Contains(reason, "branch stale") {
		t.Errorf("reason %q missing rebase-failed classification", reason)
	}
}

func TestSurfaceStartFailure_NilErrIsNoOp(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	engine.surfaceStartFailure("t1", "in-progress", nil, nil, "")

	if reason := tasks.Reason("t1"); reason != "" {
		t.Errorf("nil err wrote reason %q, want empty", reason)
	}
}

func TestSurfaceStartFailure_CircuitBreakerTripsAfterRepeatedFailures(t *testing.T) {
	// Regression guard for the flapping-task circuit breaker (sybra#1487): a
	// task whose dispatch keeps failing for the same step must eventually be
	// halted independent of status.Status alone, since something else in the
	// system (a racing recovery branch, a status-change hook) could otherwise
	// keep flipping it off human-required and letting the resume loop retry
	// forever.
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	wrapped := fmt.Errorf("prepare worktree: %w", worktreeerr.ErrRebaseFailed)
	wf := &Execution{CurrentStep: "run_test", State: ExecRunning, Variables: map[string]string{}}

	for i := range maxCircuitBreakerFailures - 1 {
		engine.surfaceStartFailure("t1", "todo", wrapped, wf, "run_test")
		if wf.State == ExecFailed {
			t.Fatalf("breaker tripped early on attempt %d, want after %d", i+1, maxCircuitBreakerFailures)
		}
	}
	engine.surfaceStartFailure("t1", "todo", wrapped, wf, "run_test")

	if wf.State != ExecFailed {
		t.Errorf("wf.State = %q, want ExecFailed after %d failures", wf.State, maxCircuitBreakerFailures)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status != "human-required" {
		t.Errorf("status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "circuit breaker") {
		t.Errorf("reason %q missing circuit-breaker classification", reason)
	}
}

// TestSurfaceStartFailure_TransientRateLimitDoesNotTripBreaker guards sybra#1585:
// an all-providers-throttled gate error (provider.UnhealthyError, rate_limited)
// is a transient capacity condition that self-heals when the cooldown expires,
// so it must never feed the circuit breaker or escalate the task, no matter how
// many times it repeats within the window. Auth failures keep tripping it.
func TestSurfaceStartFailure_TransientRateLimitDoesNotTripBreaker(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	rateLimited := &provider.UnhealthyError{Provider: "claude", Reason: provider.RateLimitReason}
	wf := &Execution{CurrentStep: "implement", State: ExecRunning, Variables: map[string]string{}}

	for i := range maxCircuitBreakerFailures + 2 {
		engine.surfaceStartFailure("t1", "in-progress", rateLimited, wf, "implement")
		if wf.State == ExecFailed {
			t.Fatalf("transient rate limit tripped the breaker on attempt %d", i+1)
		}
	}
	if _, recorded := wf.Variables[circuitBreakerFailureKey("implement")]; recorded {
		t.Fatalf("transient rate limit recorded a breaker failure: %v", wf.Variables)
	}
	got, _ := tasks.GetTask("t1")
	if got.Status == "human-required" {
		t.Fatal("transient rate limit escalated to human-required")
	}
}

func TestSurfaceStartFailure_CircuitBreakerResetsAfterWindow(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	wrapped := fmt.Errorf("prepare worktree: %w", worktreeerr.ErrRebaseFailed)
	old := time.Now().Add(-2 * circuitBreakerWindow).Format(time.RFC3339)
	wf := &Execution{
		CurrentStep: "run_test",
		State:       ExecRunning,
		Variables: map[string]string{
			circuitBreakerFirstKey("run_test"):   old,
			circuitBreakerFailureKey("run_test"): strconv.Itoa(maxCircuitBreakerFailures),
		},
	}

	engine.surfaceStartFailure("t1", "todo", wrapped, wf, "run_test")

	if wf.State == ExecFailed {
		t.Error("breaker tripped using a stale, expired failure window")
	}
	if got := wf.Variables[circuitBreakerFailureKey("run_test")]; got != "1" {
		t.Errorf("failure count = %q, want reset to 1", got)
	}
}

// TestHandleAgentComplete_CircuitBreakerAttributesNextStepNotCompletedStep is
// a regression guard for the review finding on 90befcef: HandleAgentComplete
// only knows the step that just completed (spawnedStep), but the error
// AdvanceStep returns typically comes from failing to dispatch the *next*
// step. Recording the circuit-breaker failure against spawnedStep would
// misdiagnose which step is flapping and let a later successful dispatch of
// the real offender never clear it.
func TestHandleAgentComplete_CircuitBreakerAttributesNextStepNotCompletedStep(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "test-simple"); err != nil {
		t.Fatal(err)
	}
	if agents.LastCall().Role != "triage" {
		t.Fatalf("expected triage, got %q", agents.LastCall().Role)
	}
	triageAgentID := agents.LastID()

	// Arm the mock so the next dispatch (implement, reached via
	// triage -> set_in_progress -> implement) fails to spawn.
	agents.SetFailSpawn(fmt.Errorf("prepare worktree: %w", worktreeerr.ErrRebaseFailed))
	agents.SimulateComplete("t1")
	engine.HandleAgentComplete("t1", AgentCompletion{AgentID: triageAgentID, Success: true, Result: "triaged"})

	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, tripped := ti.Workflow.Variables[circuitBreakerFailureKey("implement")]; !tripped {
		t.Errorf("expected circuit breaker failure recorded against %q, vars=%v", "implement", ti.Workflow.Variables)
	}
	if _, wrong := ti.Workflow.Variables[circuitBreakerFailureKey("triage")]; wrong {
		t.Errorf("circuit breaker failure wrongly recorded against completed step %q instead of the step that failed to dispatch", "triage")
	}
}

func TestSurfaceStartFailure_AlreadyHumanRequiredIsNoOp(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "human-required", StatusReason: "existing reason"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	wrapped := fmt.Errorf("prepare worktree: %w", worktreeerr.ErrRebaseFailed)
	engine.surfaceStartFailure("t1", "human-required", wrapped, nil, "")

	got, _ := tasks.GetTask("t1")
	if got.StatusReason != "existing reason" {
		t.Errorf("StatusReason = %q, want unchanged existing reason", got.StatusReason)
	}
}
