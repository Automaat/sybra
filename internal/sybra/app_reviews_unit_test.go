package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestPRMonitorEligible exercises the scan predicate used by the PR monitor
// loop. The regression: tasks whose workflow exited to in-progress with a
// live PR number (because an evaluate step crashed, or a manually-spawned
// agent opened the PR outside the workflow) were silently dropped from the
// scan because it only considered status=in-review. Result: failing CI on
// those PRs was never fixed by pr-fix agents.
func TestPRMonitorEligible(t *testing.T) {
	tests := []struct {
		name string
		tk   task.Task
		want bool
	}{
		{
			name: "in-review with PR — original happy path",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42},
			want: true,
		},
		{
			name: "in-review with branch only — still eligible",
			tk:   task.Task{Status: task.StatusInReview, Branch: "sybra/feat-x"},
			want: true,
		},
		{
			name: "in-review with neither PR nor branch — not eligible",
			tk:   task.Task{Status: task.StatusInReview},
			want: false,
		},
		{
			name: "in-progress with PR — the regression case we're fixing",
			tk:   task.Task{Status: task.StatusInProgress, PRNumber: 247},
			want: true,
		},
		{
			name: "in-progress with branch only — not eligible (avoid WIP false positives)",
			tk:   task.Task{Status: task.StatusInProgress, Branch: "sybra/wip"},
			want: false,
		},
		{
			name: "in-progress with nothing — not eligible",
			tk:   task.Task{Status: task.StatusInProgress},
			want: false,
		},
		{
			name: "review tag excluded (inbound review task, not ours)",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42, Tags: []string{"review"}},
			want: false,
		},
		{
			name: "todo with PR — not eligible, not in monitored states",
			tk:   task.Task{Status: task.StatusTodo, PRNumber: 42},
			want: false,
		},
		{
			name: "done with PR — not eligible, already terminal",
			tk:   task.Task{Status: task.StatusDone, PRNumber: 42},
			want: false,
		},
		{
			name: "human-required with PR — not eligible, needs operator action first",
			tk:   task.Task{Status: task.StatusHumanRequired, PRNumber: 42},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prMonitorEligible(&tt.tk); got != tt.want {
				t.Errorf("prMonitorEligible(%+v) = %v, want %v", tt.tk, got, tt.want)
			}
		})
	}
}
