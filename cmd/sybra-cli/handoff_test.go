package main

import (
	"slices"
	"testing"
)

func TestHandoffStageConfigForAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		stage string
		name  string
		tags  []string
	}{
		{stage: "", name: "implement", tags: []string{"handoff"}},
		{stage: "implement", name: "implement", tags: []string{"handoff"}},
		{stage: "in-progress", name: "implement", tags: []string{"handoff"}},
		{stage: "review", name: "review", tags: []string{"handoff", "handoff-review"}},
		{stage: "ready-review", name: "review", tags: []string{"handoff", "handoff-review"}},
		{stage: "agentic-review", name: "review", tags: []string{"handoff", "handoff-review"}},
		{stage: "testing", name: "testing", tags: []string{"handoff", "handoff-testing"}},
		{stage: "ready-pr", name: "ready-pr", tags: []string{"handoff", "handoff-ready-pr"}},
		{stage: "open-pr", name: "ready-pr", tags: []string{"handoff", "handoff-ready-pr"}},
		{stage: "in-review", name: "pr", tags: []string{"review", handoffPRTag}},
		{stage: "pr", name: "pr", tags: []string{"review", handoffPRTag}},
	}

	for _, tt := range tests {
		t.Run(tt.stage, func(t *testing.T) {
			t.Parallel()
			got, ok := handoffStageConfigFor(tt.stage)
			if !ok {
				t.Fatalf("handoffStageConfigFor(%q) returned ok=false", tt.stage)
			}
			if got.name != tt.name {
				t.Fatalf("handoffStageConfigFor(%q).name = %q, want %q", tt.stage, got.name, tt.name)
			}
			if !slices.Equal(got.tags, tt.tags) {
				t.Fatalf("handoffStageConfigFor(%q).tags = %+v, want %+v", tt.stage, got.tags, tt.tags)
			}
		})
	}
}

func TestHandoffStageConfigForRejectsUnknownStage(t *testing.T) {
	t.Parallel()

	if _, ok := handoffStageConfigFor("done"); ok {
		t.Fatal("handoffStageConfigFor(\"done\") returned ok=true")
	}
}
