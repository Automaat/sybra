package sybra

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestClassifyLandingOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state string
		want  string
	}{
		{"merged", "MERGED", "merged"},
		{"closed unmerged", "CLOSED", "closed"},
		{"empty defaults to merged", "", "merged"},
		{"unknown defaults to merged", "OPEN", "merged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyLandingOutcome(tt.state); got != tt.want {
				t.Errorf("classifyLandingOutcome(%q) = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}

func TestLandingOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                       string
		state, agentSHA, mergedSHA string
		want                       string
	}{
		{"closed", "CLOSED", "abc", "def", "closed"},
		{"merged clean (same head)", "MERGED", "abc", "abc", "merged"},
		{"merged with edits (head moved)", "MERGED", "abc", "def", "merged_with_edits"},
		{"merged, no agent sha", "MERGED", "", "def", "merged"},
		{"merged, no head sha", "MERGED", "abc", "", "merged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := landingOutcome(tt.state, tt.agentSHA, tt.mergedSHA); got != tt.want {
				t.Errorf("landingOutcome(%q,%q,%q) = %q, want %q", tt.state, tt.agentSHA, tt.mergedSHA, got, tt.want)
			}
		})
	}
}

func TestLastAgentHeadSHA(t *testing.T) {
	t.Parallel()
	runs := []task.AgentRun{
		{AgentID: "a", HeadSHA: "sha-impl"},
		{AgentID: "b", HeadSHA: ""}, // e.g. an eval run with no push
		{AgentID: "c", HeadSHA: "sha-prfix"},
	}
	if got := lastAgentHeadSHA(runs); got != "sha-prfix" {
		t.Errorf("lastAgentHeadSHA = %q, want sha-prfix", got)
	}
	if got := lastAgentHeadSHA([]task.AgentRun{{AgentID: "x"}}); got != "" {
		t.Errorf("lastAgentHeadSHA with no SHAs = %q, want empty", got)
	}
}

// TestComputeLanding_LocalOnly verifies the local path: a task with no project
// produces a merged outcome with queue-inclusive timing and no network
// enrichment (no PR size, no work_to_land_h since there are no agent runs).
func TestComputeLanding_LocalOnly(t *testing.T) {
	taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("task NewStore: %v", err)
	}
	tasks := task.NewManager(taskStore, nil)
	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		tasks:         tasks,
	}
	outcome, data := r.computeLanding(created.ID, 42, "MERGED", "merged")

	if outcome != "merged" {
		t.Errorf("outcome = %q, want merged", outcome)
	}
	if data["outcome"] != "merged" || data["pr"] != 42 {
		t.Errorf("data outcome/pr = %v/%v", data["outcome"], data["pr"])
	}
	if _, ok := data["created_to_land_h"]; !ok {
		t.Errorf("missing created_to_land_h in %v", data)
	}
	if _, ok := data["work_to_land_h"]; ok {
		t.Errorf("unexpected work_to_land_h for task with no agent runs: %v", data)
	}
	if _, ok := data["agent_head_sha"]; ok {
		t.Errorf("unexpected agent_head_sha for task with no captured SHA: %v", data)
	}
	if _, ok := data["additions"]; ok {
		t.Errorf("unexpected PR-size enrichment for project-less task: %v", data)
	}
}
