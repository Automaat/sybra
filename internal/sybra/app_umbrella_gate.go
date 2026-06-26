package sybra

import (
	"slices"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

// umbrellaGatedTag is the tag the expander sets on a gate-blocked child; the
// gate requires it to release and strips it on release. Aliased from the
// umbrella package so the CLI expander and this gate cannot drift.
const umbrellaGatedTag = umbrella.GatedTag

// umbrellaState aggregates one umbrella's tracker and children for a gate tick.
type umbrellaState struct {
	tracker   *task.Task
	cap       int // max children running at once
	total     int // child task count
	doneCount int
	active    int // children occupying a parallelism slot
	anyHR     bool
	released  int // children released so far this tick (counts toward the cap)
}

// releaseUnblockedChildren is the umbrella gate, run every orchestrator tick.
// It releases gate-blocked children whose dependencies are done — up to each
// umbrella's max-parallel cap — and rolls each umbrella tracker's status up
// from its children (cycle or a stuck child → human-required; all done →
// done + close the umbrella issue). No-op when no umbrella tasks exist.
func (a *App) releaseUnblockedChildren() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}

	byID := make(map[string]*task.Task, len(tasks))
	states := map[string]*umbrellaState{}
	nodes := make([]umbrella.Node, len(tasks))
	hasUmbrella := false

	stateFor := func(ref string) *umbrellaState {
		key := umbrella.NormalizeIssueRef(ref)
		s := states[key]
		if s == nil {
			s = &umbrellaState{cap: umbrella.DefaultMaxParallel}
			states[key] = s
		}
		return s
	}

	for i := range tasks {
		t := &tasks[i]
		byID[t.ID] = t
		if t.TaskType == task.TaskTypeUmbrella {
			hasUmbrella = true
			st := stateFor(t.Issue)
			st.tracker = t
			st.cap = umbrella.ParseMaxParallel(t.Tags)
		}
		if t.UmbrellaIssue != "" {
			hasUmbrella = true
			accumulateChild(stateFor(t.UmbrellaIssue), t.Status)
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
	cyclic := map[string]bool{}
	for _, umb := range g.CyclicUmbrellas() {
		cyclic[umbrella.NormalizeIssueRef(umb)] = true
	}

	a.releaseCapped(g.ReadyToRelease(), byID, states)
	a.rollupTrackers(states, cyclic)
}

// accumulateChild folds one child task's status into its umbrella's tally.
func accumulateChild(st *umbrellaState, status task.Status) {
	st.total++
	switch status {
	case task.StatusDone:
		st.doneCount++
	case task.StatusHumanRequired:
		st.anyHR = true
		st.active++ // a stuck child still occupies a slot until resolved
	default:
		if isRunningChild(status) {
			st.active++
		}
	}
}

// isRunningChild reports whether a child status occupies a parallelism slot —
// i.e. it has been released and is somewhere in the pipeline but not finished.
func isRunningChild(s task.Status) bool {
	switch s {
	case task.StatusBlocked, task.StatusNew, task.StatusDone, task.StatusCancelled:
		return false
	default:
		return true
	}
}

// releaseCapped releases ready children to `todo`, but no more than each
// umbrella's remaining parallelism budget (cap - active). Strips the gating
// marker on release so a later re-block cannot retrigger it.
func (a *App) releaseCapped(ready []string, byID map[string]*task.Task, states map[string]*umbrellaState) {
	for _, id := range ready {
		t, ok := byID[id]
		if !ok || t == nil {
			continue
		}
		st := states[umbrella.NormalizeIssueRef(t.UmbrellaIssue)]
		if st == nil || st.active+st.released >= st.cap {
			continue // at the umbrella's parallelism cap for this tick
		}
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
		st.released++
		a.logger.Info("umbrella.child.released", "task_id", id)
	}
}

// rollupTrackers advances each umbrella tracker's status to reflect its
// children and closes the umbrella issue when everything is done.
func (a *App) rollupTrackers(states map[string]*umbrellaState, cyclic map[string]bool) {
	for key, st := range states {
		if st.tracker == nil {
			continue
		}
		desired, reason, doClose := trackerRollup(st, cyclic[key])
		if desired == st.tracker.Status {
			continue
		}
		if _, err := a.tasks.Update(st.tracker.ID, task.Update{
			Status:       task.Ptr(desired),
			StatusReason: task.Ptr(reason),
		}); err != nil {
			a.logger.Error("umbrella.tracker.update.failed", "task_id", st.tracker.ID, "err", err)
			continue
		}
		a.logger.Info("umbrella.tracker.rollup", "task_id", st.tracker.ID, "status", desired)
		if doClose {
			a.closeUmbrellaIssue(st.tracker.Issue)
		}
	}
}

// trackerRollup decides an umbrella tracker's status from its children. A
// cycle or a stuck (human-required) child surfaces as human-required and halts
// only that chain; all-done closes the umbrella.
func trackerRollup(st *umbrellaState, cyclic bool) (status task.Status, reason string, doClose bool) {
	switch {
	case cyclic:
		return task.StatusHumanRequired, "umbrella dependency cycle detected", false
	case st.anyHR:
		return task.StatusHumanRequired, "umbrella child needs attention", false
	case st.total > 0 && st.doneCount == st.total:
		return task.StatusDone, "all umbrella children complete", true
	default:
		return task.StatusInProgress, "umbrella in progress", false
	}
}

// closeUmbrellaIssue closes the umbrella's GitHub issue with a generic comment
// (no work content). Best-effort: a failure is logged, not retried.
func (a *App) closeUmbrellaIssue(umbRef string) {
	repo, number, ok := umbrella.ParseRef(umbRef)
	if !ok {
		a.logger.Warn("umbrella.close.skip", "reason", "unparseable ref", "ref", umbRef)
		return
	}
	closeFn := a.umbrellaCloseIssue
	if closeFn == nil {
		closeFn = github.CloseIssue
	}
	if err := closeFn(repo, number, "All umbrella sub-tasks completed."); err != nil {
		a.logger.Error("umbrella.close.failed", "repo", repo, "number", number, "err", err)
		return
	}
	a.logger.Info("umbrella.closed", "repo", repo, "number", number)
}
