package workflow

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestPredictedRestingState(t *testing.T) {
	t.Parallel()

	before := &Execution{CurrentStep: "start", State: ExecRunning}

	t.Run("no workflow effect keeps the pre-existing resting step", func(t *testing.T) {
		t.Parallel()
		got := predictedRestingState(before, nil)
		if got.hasWorkflow {
			t.Fatalf("hasWorkflow = true, want false when no effect touched it")
		}
		if got.currentStep != "start" || got.execState != ExecRunning {
			t.Fatalf("got = %+v, want carried-over before state", got)
		}
	})

	t.Run("workflow effect overrides resting step, last one wins", func(t *testing.T) {
		t.Parallel()
		effects := []Effect{
			{Kind: EffectSetWorkflowState, Workflow: &Execution{CurrentStep: "mid", State: ExecRunning}},
			{Kind: EffectSetWorkflowState, Workflow: &Execution{CurrentStep: "end", State: ExecWaiting}},
		}
		got := predictedRestingState(before, effects)
		if !got.hasWorkflow || got.currentStep != "end" || got.execState != ExecWaiting {
			t.Fatalf("got = %+v, want end/waiting", got)
		}
	})

	t.Run("set task status effect is captured independently of workflow effect", func(t *testing.T) {
		t.Parallel()
		effects := []Effect{
			{Kind: EffectSetTaskStatus, Status: string(taskstatus.HumanRequired), StatusReason: "blocked"},
		}
		got := predictedRestingState(before, effects)
		if got.hasWorkflow {
			t.Fatalf("hasWorkflow = true, want false: no workflow effect present")
		}
		if !got.hasStatus || got.status != string(taskstatus.HumanRequired) || got.statusReason != "blocked" {
			t.Fatalf("got = %+v, want human-required/blocked", got)
		}
	})
}

func TestDiffShadowState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		predicted shadowRestingState
		actual    TaskInfo
		wantEmpty bool
		wantHas   []string // substrings expected in the diff
	}{
		{
			name:      "no claims made means no diff",
			predicted: shadowRestingState{},
			actual:    TaskInfo{Status: taskstatus.InProgress},
			wantEmpty: true,
		},
		{
			name: "matching workflow state is not a divergence",
			predicted: shadowRestingState{
				hasWorkflow: true, currentStep: "review", execState: ExecWaiting,
			},
			actual:    TaskInfo{Workflow: &Execution{CurrentStep: "review", State: ExecWaiting}},
			wantEmpty: true,
		},
		{
			name: "diverging current step is reported",
			predicted: shadowRestingState{
				hasWorkflow: true, currentStep: "review", execState: ExecWaiting,
			},
			actual:  TaskInfo{Workflow: &Execution{CurrentStep: "implement", State: ExecWaiting}},
			wantHas: []string{`current_step: predicted="review" actual="implement"`},
		},
		{
			name: "diverging task status is reported",
			predicted: shadowRestingState{
				hasStatus: true, status: string(taskstatus.HumanRequired), statusReason: "budget exhausted",
			},
			actual:  TaskInfo{Status: taskstatus.InProgress, StatusReason: ""},
			wantHas: []string{`status: predicted="human-required" actual="in-progress"`, "status_reason"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := diffShadowState(tt.predicted, tt.actual)
			if tt.wantEmpty && got != "" {
				t.Fatalf("diff = %q, want empty", got)
			}
			for _, want := range tt.wantHas {
				if !strings.Contains(got, want) {
					t.Fatalf("diff = %q, want substring %q", got, want)
				}
			}
		})
	}
}
