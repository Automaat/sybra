package main

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestStatusListForUsageMatchesAllStatuses pins the `list --status` help
// text to task.AllStatuses() so the CLI usage string can never drift from
// the Status enum again — there is only one list to edit.
func TestStatusListForUsageMatchesAllStatuses(t *testing.T) {
	t.Parallel()

	statuses := task.AllStatuses()
	want := make([]string, len(statuses))
	for i, s := range statuses {
		want[i] = string(s)
	}

	got := strings.Split(statusListForUsage(), "|")
	if len(got) != len(want) {
		t.Fatalf("statusListForUsage() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statusListForUsage()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
