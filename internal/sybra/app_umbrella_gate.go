package sybra

import (
	"slices"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

// umbrellaGatedTag marks a child task that is held in `blocked` specifically by
// the umbrella dependency gate (set by the expander when the child is
// materialized). It distinguishes umbrella gating from the unrelated `blocked`
// the human-review automation uses for a contained Sybra bug, so the gate only
// releases tasks it is actually responsible for. The gate strips the tag on
// release, so a child that later re-enters `blocked` for a different reason is
// never re-released.
const umbrellaGatedTag = "umbrella-gated"

// releaseUnblockedChildren scans umbrella child tasks held in `blocked` by the
// gate and releases those whose dependencies have all reached `done`, in
// dependency order. Children caught in a dependency cycle are never released;
// their umbrella tracker is flipped to human-required instead. Runs every
// orchestrator tick and is a no-op when no umbrella tasks exist.
func (a *App) releaseUnblockedChildren() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}

	byID := make(map[string]*task.Task, len(tasks))
	nodes := make([]umbrella.Node, len(tasks))
	hasUmbrella := false
	for i := range tasks {
		t := &tasks[i]
		byID[t.ID] = t
		if t.UmbrellaIssue != "" || t.TaskType == task.TaskTypeUmbrella {
			hasUmbrella = true
		}
		nodes[i] = umbrella.Node{
			ID:        t.ID,
			Issue:     t.Issue,
			Umbrella:  t.UmbrellaIssue,
			DependsOn: t.DependsOn,
			Done:      t.Status == task.StatusDone,
			// Only a task the gate itself blocked is eligible for release —
			// never one parked in `blocked` for a contained Sybra bug.
			Awaiting: t.UmbrellaIssue != "" && t.Status == task.StatusBlocked &&
				slices.Contains(t.Tags, umbrellaGatedTag),
		}
	}
	if !hasUmbrella {
		return
	}

	g := umbrella.Build(nodes)

	for _, umb := range g.CyclicUmbrellas() {
		a.flagCyclicUmbrella(tasks, umb)
	}

	for _, id := range g.ReadyToRelease() {
		t, ok := byID[id]
		if !ok || t == nil {
			continue
		}
		// Strip the gating marker on release so a later re-block (e.g. a
		// Sybra-bug containment) cannot retrigger a release of the same task.
		newTags := slices.DeleteFunc(slices.Clone(t.Tags), func(s string) bool {
			return s == umbrellaGatedTag
		})
		if _, err := a.tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusTodo),
			Tags:         &newTags,
			StatusReason: task.Ptr("umbrella dependencies satisfied"),
		}); err != nil {
			a.logger.Error("umbrella.release.failed", "task_id", id, "err", err)
			continue
		}
		a.logger.Info("umbrella.child.released", "task_id", id)
	}
}

// flagCyclicUmbrella flips the umbrella tracker for umb to human-required when
// its children form a dependency cycle. Idempotent: a tracker already in
// human-required is left untouched so the gate does not churn the file every
// tick.
func (a *App) flagCyclicUmbrella(tasks []task.Task, umb string) {
	key := umbrella.NormalizeIssueRef(umb)
	for i := range tasks {
		t := &tasks[i]
		if t.TaskType != task.TaskTypeUmbrella || umbrella.NormalizeIssueRef(t.Issue) != key {
			continue
		}
		if t.Status == task.StatusHumanRequired {
			return
		}
		if _, err := a.tasks.Update(t.ID, task.Update{
			Status:       task.Ptr(task.StatusHumanRequired),
			StatusReason: task.Ptr("umbrella dependency cycle detected"),
		}); err != nil {
			a.logger.Error("umbrella.cycle.flag.failed", "task_id", t.ID, "err", err)
		}
		return
	}
}
