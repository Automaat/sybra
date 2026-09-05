package task_test

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestHandoffVariantsEnterFromTodo(t *testing.T) {
	// Given every handoff entry point Sybra ships
	defs, err := workflow.BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	entries := map[string]string{}
	for i := range defs {
		def := &defs[i]
		if !strings.HasPrefix(def.ID, "simple-task-handoff") {
			continue
		}
		if len(def.Steps) == 0 || def.Steps[0].Type != workflow.StepSetStatus {
			continue
		}
		entries[def.ID] = def.Steps[0].Config.Status
	}
	if len(entries) < 4 {
		t.Fatalf("found %d handoff entry points, want every shipped stage", len(entries))
	}

	for id, status := range entries {
		t.Run(id, func(t *testing.T) {
			// When a handed-off task is created at todo and flipped to its stage
			// Then the move is legal, so the stage is reachable instead of
			// tripping the circuit breaker three dispatches later
			if !task.IsTransitionAllowed(task.StatusTodo, task.Status(status)) {
				t.Fatalf("handoff %s flips todo -> %q, which the transition table refuses", id, status)
			}
		})
	}
}
