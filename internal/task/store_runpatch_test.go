package task

import "testing"

func TestApplyRunPatch_ToolFailuresReachTheRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		start int
		patch *int
		want  int
	}{
		{name: "an unset patch leaves the count alone", start: 3, want: 3},
		{name: "a clean run records zero", patch: Ptr(0)},
		{name: "a lossy run records its count", patch: Ptr(7), want: 7},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := AgentRun{ToolFailures: tc.start}
			applyRunCostTokens(&run, RunPatch{ToolFailures: tc.patch})
			if run.ToolFailures != tc.want {
				t.Errorf("ToolFailures = %d, want %d", run.ToolFailures, tc.want)
			}
		})
	}
}
