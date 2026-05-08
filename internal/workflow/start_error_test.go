package workflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
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

func TestSurfaceStartFailure_NilErrIsNoOp(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(nil, tasks, newMockAgents(), discardLogger())

	engine.surfaceStartFailure("t1", "in-progress", nil)

	if reason := tasks.Reason("t1"); reason != "" {
		t.Errorf("nil err wrote reason %q, want empty", reason)
	}
}
