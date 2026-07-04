package workflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"

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

func TestSurfaceStartFailure_DispatchInFlightIsNoOp(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	engine.surfaceStartFailure("t1", "in-progress", ErrDispatchInFlight)

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

	engine.surfaceStartFailure("t1", "in-progress", errors.New("git fetch: timeout"))

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
	engine.surfaceStartFailure("t1", "in-progress", wrapped)

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
	engine.surfaceStartFailure("t1", "in-progress", wrapped)

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
	engine.surfaceStartFailure("t1", "in-progress", wrapped)

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

	engine.surfaceStartFailure("t1", "in-progress", nil)

	if reason := tasks.Reason("t1"); reason != "" {
		t.Errorf("nil err wrote reason %q, want empty", reason)
	}
}
