package sybra

import (
	"slices"

	"github.com/Automaat/sybra/internal/task"
)

const (
	handoffManualTag = "handoff-manual"
	handoffPRTag     = "handoff-pr"
)

func skipTaskCreatedWorkflow(t task.Task) bool {
	if slices.Contains(t.Tags, handoffManualTag) {
		return true
	}
	if t.RunRole != "" {
		return true
	}
	if t.PRNumber > 0 && !allowsTaskCreatedWorkflowWithPR(t) {
		return true
	}
	return false
}

func allowsTaskCreatedWorkflowWithPR(t task.Task) bool {
	return slices.Contains(t.Tags, "handoff") || slices.Contains(t.Tags, handoffPRTag)
}
