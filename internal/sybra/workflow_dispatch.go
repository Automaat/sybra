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
	// umbrellaDuplicateTag marks a stub whose umbrellaExpand succeeded (a real
	// tracker + gated children already exist elsewhere) but whose own DeleteTask
	// cleanup failed. It is deliberately distinct from TaskTypeUmbrella: this
	// task is not a tracker and must never be treated as one by
	// app_umbrella_gate.go's rollup loop or umbrella.scanExisting's re-expansion
	// lookup, both of which key on TaskTypeUmbrella with last-match-wins
	// semantics. This tag is the durable, collision-free dispatch guard for it.
	umbrellaDuplicateTag = "umbrella-duplicate"
)

func skipTaskCreatedWorkflow(t task.Task) bool {
	// An umbrella tracker runs no agent — it only rolls up its children — so it
	// must never trigger the plan/implement pipeline.
	if t.TaskType == task.TaskTypeUmbrella {
		return true
	}
	// A duplicate stub left behind when post-expansion cleanup fails: a real
	// tracker already exists for this issue elsewhere, so this task must never
	// claim TaskTypeUmbrella itself (that would collide with the real
	// tracker's identity in app_umbrella_gate.go/scanExisting) — but it still
	// must never flat-dispatch. This marker is the durable guard for that case.
	if slices.Contains(t.Tags, umbrellaDuplicateTag) {
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
