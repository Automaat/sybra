package sybra

import (
	"slices"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

const (
	handoffManualTag = "handoff-manual"
	// enrichPendingTag marks a stub created from a raw GitHub issue/PR URL
	// whose real title/body/labels are still being fetched asynchronously
	// (TaskService.enrichFromIssue/enrichFromPR). It is set atomically at
	// creation so the emit-path task.created dispatch
	// (maybeStartWorkflowForExternalTask) cannot race enrichment and start a
	// flat workflow on the un-enriched stub — notably an umbrella that must be
	// expanded, not implemented. The enrich step clears it and then dispatches.
	// CLI-created URL tasks use plain Create (no marker), so they keep going
	// through task.created → triage, which fetches the title itself.
	enrichPendingTag = "enrich-pending"
)

func skipTaskCreatedWorkflow(t task.Task) bool {
	// An umbrella tracker runs no agent — it only rolls up its children — so it
	// must never trigger the plan/implement pipeline.
	if t.TaskType == task.TaskTypeUmbrella {
		return true
	}
	// A stub awaiting async enrichment is dispatched by the enrich step, not
	// the emit path — skip until the real title/labels land.
	if slices.Contains(t.Tags, enrichPendingTag) {
		return true
	}
	// An umbrella-gated child must be held for releaseUnblockedChildren, not
	// dispatched straight into triage/plan/implement.
	if slices.Contains(t.Tags, umbrella.GatedTag) {
		return true
	}
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
	return slices.Contains(t.Tags, "handoff")
}
