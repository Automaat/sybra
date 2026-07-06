package sybra

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

// umbrellaGatedTag is the tag the expander sets on a gate-blocked child; the
// gate requires it to release and strips it on release. Aliased from the
// umbrella package so the CLI expander and this gate cannot drift.
const umbrellaGatedTag = umbrella.GatedTag

// umbrellaSettleDelay is how long after creation a childless umbrella tracker
// must persist before the gate treats it as complete. Comfortably exceeds the
// 1-minute orchestrator tick so a tracker whose children are still being
// materialized in the same expansion is never closed prematurely.
const umbrellaSettleDelay = 2 * time.Minute

// umbrellaState aggregates one umbrella's tracker and children for a gate tick.
type umbrellaState struct {
	tracker      *task.Task
	cap          int // max children running at once
	total        int // child task count
	doneCount    int
	active       int // children occupying a parallelism slot
	anyHR        bool
	anyCancelled bool
	released     int // children released so far this tick (counts toward the cap)
	children     []umbrellaProgressChild
}

type umbrellaProgressChild struct {
	id     string
	title  string
	issue  string
	status task.Status
}

const (
	umbrellaProgressStart = "<!-- sybra:umbrella-progress:start -->"
	umbrellaProgressEnd   = "<!-- sybra:umbrella-progress:end -->"
)

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
			accumulateChild(stateFor(t.UmbrellaIssue), t)
		}
		nodes[i] = umbrella.Node{
			ID:        t.ID,
			Issue:     t.Issue,
			Umbrella:  t.UmbrellaIssue,
			DependsOn: t.DependsOn,
			Done:      t.Status == task.StatusDone,
			// Gate-marked todo children (current model) and legacy
			// blocked+gated children (tasks created before this change)
			// are both eligible for release. Never release a task that is
			// blocked without the gating tag (contained Sybra bug).
			Awaiting: t.UmbrellaIssue != "" &&
				(t.Status == task.StatusTodo || t.Status == task.StatusBlocked) &&
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

	// A blocked tracker pauses only tracker rollup/issue close; dependency-ready
	// children still release so independent work under the umbrella can proceed.
	a.releaseCapped(g.ReadyToRelease(), byID, states)
	a.rollupTrackers(states, cyclic)
}

// accumulateChild folds one child task's status into its umbrella's tally.
// A todo child carrying the gating tag is still waiting for deps — it must not
// count as active or it would consume a parallelism slot before being released.
func accumulateChild(st *umbrellaState, t *task.Task) {
	st.total++
	st.children = append(st.children, umbrellaProgressChild{
		id:     t.ID,
		title:  t.Title,
		issue:  t.Issue,
		status: t.Status,
	})
	switch t.Status {
	case task.StatusDone:
		st.doneCount++
	case task.StatusCancelled:
		// A cancelled prerequisite is a deliberate abandonment — surface it for
		// a human rather than silently completing the umbrella or proceeding on
		// the cancelled work (its dependents stay held; see depsSatisfied).
		st.anyCancelled = true
	case task.StatusHumanRequired:
		st.anyHR = true
		st.active++ // a stuck child still occupies a slot until resolved
	default:
		if isRunningChild(t.Status) && !slices.Contains(t.Tags, umbrellaGatedTag) {
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
		updated, err := a.tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusTodo),
			Tags:         &newTags,
			StatusReason: task.Ptr("umbrella dependencies satisfied"),
		})
		if err != nil {
			a.logger.Error("umbrella.release.failed", "task_id", id, "err", err)
			continue
		}
		st.setChildStatus(id, updated.Status)
		st.released++
		a.logger.Info("umbrella.child.released", "task_id", id)
	}
}

func (st *umbrellaState) setChildStatus(id string, status task.Status) {
	for i := range st.children {
		if st.children[i].id == id {
			st.children[i].status = status
			return
		}
	}
}

// rollupTrackers advances each umbrella tracker's status to reflect its
// children and closes the umbrella issue when everything is done.
func (a *App) rollupTrackers(states map[string]*umbrellaState, cyclic map[string]bool) {
	for key, st := range states {
		if st.tracker == nil {
			continue
		}
		if st.tracker.Status == task.StatusBlocked {
			continue
		}
		// A tracker is "settled" once it has outlived the creation window, so a
		// childless tally that just reflects children still being materialized
		// is not mistaken for a completed umbrella. A zero CreatedAt (e.g. a
		// task file missing created_at) is treated as not settled rather than
		// infinitely old, so it never bypasses the guard.
		settled := !st.tracker.CreatedAt.IsZero() &&
			time.Since(st.tracker.CreatedAt) > umbrellaSettleDelay
		desired, reason, doClose := trackerRollup(st, cyclic[key], settled)
		if body := umbrellaTrackerBody(st.tracker.Body, st.children); body != st.tracker.Body {
			if _, err := a.tasks.Update(st.tracker.ID, task.Update{
				Body: task.Ptr(body),
			}); err != nil {
				a.logger.Error("umbrella.tracker.progress.update.failed", "task_id", st.tracker.ID, "err", err)
				continue
			}
		}
		if desired == st.tracker.Status {
			continue
		}
		// Close the umbrella issue BEFORE flipping the tracker to done, so a
		// transient close failure is retried next tick rather than leaving the
		// issue open under a done tracker that never re-attempts the close.
		if doClose && a.closeUmbrellaIssue(st.tracker.Issue) {
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
	}
}

func umbrellaTrackerBody(body string, children []umbrellaProgressChild) string {
	block := renderUmbrellaProgressBlock(children)
	start := strings.Index(body, umbrellaProgressStart)
	if start >= 0 {
		searchFrom := start + len(umbrellaProgressStart)
		if relEnd := strings.Index(body[searchFrom:], umbrellaProgressEnd); relEnd >= 0 {
			end := searchFrom + relEnd + len(umbrellaProgressEnd)
			return body[:start] + block + body[end:]
		}
		return body[:start] + block
	}
	if strings.TrimSpace(body) == "" {
		return block
	}
	sep := "\n\n"
	if strings.HasSuffix(body, "\n\n") {
		sep = ""
	} else if strings.HasSuffix(body, "\n") {
		sep = "\n"
	}
	return body + sep + block
}

func renderUmbrellaProgressBlock(children []umbrellaProgressChild) string {
	children = slices.Clone(children)
	slices.SortFunc(children, func(a, b umbrellaProgressChild) int {
		aIssue := umbrella.NormalizeIssueRef(a.issue)
		bIssue := umbrella.NormalizeIssueRef(b.issue)
		if aIssue != bIssue {
			return strings.Compare(aIssue, bIssue)
		}
		return strings.Compare(a.title, b.title)
	})

	var b strings.Builder
	b.WriteString(umbrellaProgressStart)
	b.WriteString("\n## Subissues\n\n")
	if len(children) == 0 {
		b.WriteString("_No materialized subissues._\n")
	} else {
		for _, child := range children {
			box := " "
			if child.status == task.StatusDone {
				box = "x"
			}
			fmt.Fprintf(&b, "- [%s] %s%s — %s\n",
				box,
				strings.ReplaceAll(child.title, "\n", " "),
				umbrellaProgressIssueSuffix(child.issue),
				child.status,
			)
		}
	}
	b.WriteString(umbrellaProgressEnd)
	return b.String()
}

func umbrellaProgressIssueSuffix(ref string) string {
	_, n, ok := umbrella.ParseRef(ref)
	if ok {
		return fmt.Sprintf(" (#%d)", n)
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	return " (" + ref + ")"
}

// trackerRollup decides an umbrella tracker's status from its children. A
// cycle, a stuck (human-required) child, or a cancelled child surfaces as
// human-required (halting only that chain); all-done closes the umbrella. A
// tracker with no children (every sub-issue was already closed at expansion)
// is vacuously complete, but only once `settled` (so a tracker observed while
// its children are still being materialized is not closed prematurely) and
// only when expansion isn't currently failing — a tracker carrying
// umbrella.ExpandFailTagPrefix (see internal/umbrella.recordExpandFailure)
// never had a chance to materialize its children in the first place, and
// closing it would silently drop the umbrella issue while sub-issues remain
// open on GitHub (#1570).
func trackerRollup(st *umbrellaState, cyclic, settled bool) (status task.Status, reason string, doClose bool) {
	expandFailing := st.tracker != nil && umbrella.ParseExpandFailCount(st.tracker.Tags) > 0
	switch {
	case cyclic:
		return task.StatusHumanRequired, "umbrella dependency cycle detected", false
	case st.anyHR:
		return task.StatusHumanRequired, "umbrella child needs attention", false
	case st.anyCancelled:
		return task.StatusHumanRequired, "umbrella child was cancelled", false
	case st.total > 0 && st.doneCount == st.total:
		return task.StatusDone, "all umbrella children complete", true
	case st.total == 0 && expandFailing:
		// Defer entirely to internal/umbrella.recordExpandFailure, which owns
		// this tracker's status/reason while expansion keeps failing (staying
		// in-progress below ExpandFailThreshold, human-required at it). A
		// desired value computed here would fight that state every tick —
		// e.g. flipping a parked human-required tracker straight back to
		// in-progress the moment this rollup runs.
		return st.tracker.Status, st.tracker.StatusReason, false
	case st.total == 0 && settled:
		return task.StatusDone, "umbrella has no open sub-issues", true
	default:
		return task.StatusInProgress, "umbrella in progress", false
	}
}

// closeUmbrellaIssue closes the umbrella's GitHub issue with a generic comment
// (no work content). It returns retry=true on any close failure so the caller
// holds off flipping the tracker to done and tries again next tick (gh issue
// close is idempotent, so re-attempts are safe); a persistently failing close
// leaves the tracker in-progress for the operator to notice. An unparseable
// ref is permanent (retry=false): the tracker still completes, the issue is
// just left for manual closing.
func (a *App) closeUmbrellaIssue(umbRef string) (retry bool) {
	repo, number, ok := umbrella.ParseRef(umbRef)
	if !ok {
		a.logger.Warn("umbrella.close.skip", "reason", "unparseable ref", "ref", umbRef)
		return false
	}
	closeFn := a.umbrellaCloseIssue
	if closeFn == nil {
		closeFn = github.CloseIssue
	}
	if err := closeFn(repo, number, "All umbrella sub-tasks completed."); err != nil {
		a.logger.Error("umbrella.close.failed", "repo", repo, "number", number, "err", err)
		return true
	}
	a.logger.Info("umbrella.closed", "repo", repo, "number", number)
	return false
}

// buildGroundLister returns a TrackedFilesFunc backed by projStore's existing
// bare clones: it resolves repo -> registered project -> clone's default
// branch -> tracked files at that branch, with no network fetch. Any
// resolution failure (unregistered repo, unreadable clone) is returned to the
// caller so grounding fails open rather than silently skipping.
func buildGroundLister(projStore *project.Store) umbrella.TrackedFilesFunc {
	return func(ctx context.Context, repo string) ([]string, error) {
		p, err := projStore.Get(repo)
		if err != nil {
			return nil, fmt.Errorf("ground: project %s: %w", repo, err)
		}
		files, err := project.TrackedFilesAtDefaultBranch(ctx, p.ClonePath)
		if err != nil {
			return nil, fmt.Errorf("ground: tracked files for %s: %w", repo, err)
		}
		return files, nil
	}
}
