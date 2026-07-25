package sybra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/clusterlead"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

var errTestClose = errors.New("close failed")

// mkTracker creates an umbrella tracker task carrying the given parallelism cap.
func mkTracker(t *testing.T, m *task.Manager, umb string, maxPar int) task.Task {
	t.Helper()
	tk, err := m.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
		Tags:     task.Ptr([]string{"umbrella", umbrella.MaxParallelTag(maxPar)}),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}
	return tk
}

func TestReleaseUnblockedChildren_RespectsMaxParallel(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	mkTracker(t, m, umb, 2)

	c1 := mkChild(t, m, "c1", "Automaat/sybra#1", umb, nil, task.StatusTodo)
	c2 := mkChild(t, m, "c2", "Automaat/sybra#2", umb, nil, task.StatusTodo)
	c3 := mkChild(t, m, "c3", "Automaat/sybra#3", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	released, held := 0, 0
	for _, id := range []string{c1.ID, c2.ID, c3.ID} {
		tk := mustTask(t, m, id)
		if tk.Status != task.StatusTodo {
			continue
		}
		if slices.Contains(tk.Tags, umbrellaGatedTag) {
			held++
		} else {
			released++
		}
	}
	if released != 2 || held != 1 {
		t.Fatalf("cap=2 should release 2 and hold 1, got released=%d held=%d", released, held)
	}
}

func TestReleaseUnblockedChildren_HaltChainFlagsTracker(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)

	// One child is stuck needing a human; an unrelated child is ready.
	stuck := mkChild(t, m, "stuck", "Automaat/sybra#1", umb, nil, task.StatusHumanRequired)
	indep := mkChild(t, m, "indep", "Automaat/sybra#2", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want human-required on stuck child", got)
	}
	if got := mustStatus(t, m, stuck.ID); got != task.StatusHumanRequired {
		t.Fatalf("stuck child = %q, want human-required", got)
	}
	// Independent chains keep running.
	if got := mustStatus(t, m, indep.ID); got != task.StatusTodo {
		t.Fatalf("independent child = %q, want released to todo", got)
	}
}

func TestReleaseUnblockedChildren_StuckChildDoesNotConsumeParallelSlot(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 1)

	stuck, err := m.CreateFull("stuck", "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr("Automaat/sybra#1"),
		UmbrellaIssue: task.Ptr(umb),
		Status:        task.Ptr(task.StatusHumanRequired),
	})
	if err != nil {
		t.Fatalf("create stuck child: %v", err)
	}
	ready := mkChild(t, m, "ready", "Automaat/sybra#2", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want human-required on stuck child", got)
	}
	if got := mustStatus(t, m, stuck.ID); got != task.StatusHumanRequired {
		t.Fatalf("stuck child = %q, want human-required", got)
	}
	readyTask := mustTask(t, m, ready.ID)
	if readyTask.Status != task.StatusTodo || slices.Contains(readyTask.Tags, umbrellaGatedTag) {
		t.Fatalf("ready child was not released despite free slot: status=%q tags=%v", readyTask.Status, readyTask.Tags)
	}
}

func TestReleaseUnblockedChildren_RetryableWatchdogStopKeepsTrackerInProgress(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 2)

	stopped, err := m.CreateFull("stopped", "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr("Automaat/sybra#1"),
		UmbrellaIssue: task.Ptr(umb),
		Status:        task.Ptr(task.StatusHumanRequired),
		StatusReason:  task.Ptr("watchdog: loop stop: Agent re-running failing test `go test ./cmd/sybra-cli` despite no code change"),
	})
	if err != nil {
		t.Fatalf("create stopped child: %v", err)
	}
	ready := mkChild(t, m, "ready", "Automaat/sybra#2", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	if got := mustStatus(t, m, tracker.ID); got != task.StatusInProgress {
		t.Fatalf("tracker = %q, want in-progress while watchdog-stopped child is still recoverable", got)
	}
	if got := mustStatus(t, m, stopped.ID); got != task.StatusHumanRequired {
		t.Fatalf("stopped child = %q, want human-required until workflow requeues it", got)
	}
	readyTask := mustTask(t, m, ready.ID)
	if readyTask.Status != task.StatusTodo || slices.Contains(readyTask.Tags, umbrellaGatedTag) {
		t.Fatalf("ready child was not released alongside retryable watchdog stop: status=%q tags=%v", readyTask.Status, readyTask.Tags)
	}
}

// TestReleaseUnblockedChildren_NonGatedBlockedChildEscalates covers the
// stall this fix closes: a human-review flip of one child to `blocked`
// (without the umbrella-gated tag) must not freeze the whole sub-DAG at
// in-progress forever — it should surface the umbrella tracker as
// human-required, and a dependent gated child waiting on the blocked one
// must stay held (never released) rather than silently proceeding.
func TestReleaseUnblockedChildren_NonGatedBlockedChildEscalates(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)

	// Blocked by human-review, not by the umbrella gate — no gated tag.
	blocked, err := m.CreateFull("blocked child", "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr("Automaat/sybra#1"),
		UmbrellaIssue: task.Ptr(umb),
		Status:        task.Ptr(task.StatusBlocked),
	})
	if err != nil {
		t.Fatalf("create blocked child: %v", err)
	}
	dependent := mkChild(t, m, "dependent", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusBlocked)

	app.releaseUnblockedChildren(context.Background())

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want human-required on non-gated blocked child", got)
	}
	if got := mustStatus(t, m, blocked.ID); got != task.StatusBlocked {
		t.Fatalf("blocked child = %q, want to stay blocked (gate does not own it)", got)
	}
	if got := mustStatus(t, m, dependent.ID); got != task.StatusBlocked {
		t.Fatalf("dependent child = %q, want to stay held (dep never reached done)", got)
	}
}

// TestReleaseUnblockedChildren_WatchdogExhaustedBlockedChildNotReleased covers
// #2538: a child deep into implementation that watchdog rate-limit retries
// exhaust is parked `blocked` by handleWatchdogRateLimitRetry with a
// workflow-owned Blocker (blocker.KindWatchdogRateLimitExhausted,
// ActorWorkflow). Even though it still carries the umbrella-gated tag and has
// no dependencies to wait on, the gate must never treat this as its own
// dependency hold and release it — doing so discards the child's in-flight
// implementation workflow and re-triages it from scratch.
func TestReleaseUnblockedChildren_WatchdogExhaustedBlockedChildNotReleased(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)

	// CreateFull's initial-field application does not carry Blocker (only
	// Update does — mirroring the real handleWatchdogRateLimitRetry escalation,
	// which always flips an already-running child from in-progress via a
	// single UpdateTaskBlocker call, never at creation).
	stalled, err := m.CreateFull("stalled", "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr("Automaat/sybra#1"),
		UmbrellaIssue: task.Ptr(umb),
		Status:        task.Ptr(task.StatusInProgress),
		Tags:          task.Ptr([]string{umbrellaGatedTag}),
	})
	if err != nil {
		t.Fatalf("create stalled child: %v", err)
	}
	if _, err := m.Update(stalled.ID, task.Update{
		Status:       task.Ptr(task.StatusBlocked),
		StatusReason: task.Ptr("watchdog: zero-output startup retry budget exhausted after 2 identical attempts"),
		Blocker: task.Ptr(blocker.State{
			Kind:      blocker.KindWatchdogRateLimitExhausted,
			Actor:     blocker.ActorWorkflow,
			Exhausted: true,
		}),
	}); err != nil {
		t.Fatalf("escalate stalled child to blocked: %v", err)
	}

	app.releaseUnblockedChildren(context.Background())

	stalledTask := mustTask(t, m, stalled.ID)
	if stalledTask.Status != task.StatusBlocked {
		t.Fatalf("stalled child = %q, want to stay blocked (workflow-owned hold, not the gate's)", stalledTask.Status)
	}
	if !slices.Contains(stalledTask.Tags, umbrellaGatedTag) {
		t.Fatalf("stalled child tags = %v, want the gating tag left untouched since the gate never released it", stalledTask.Tags)
	}
	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want human-required to surface the stalled child", got)
	}
}

// TestReleaseUnblockedChildren_WatchdogBlockedChildWithImplementationHistoryNotReleased
// is the regression guard for sybra#2538: a child that already ran an
// implementation agent and later hit `blocked` via watchdog exhaustion must
// never be re-released as though it were still awaiting its dependencies,
// even if it still (or once again) carries the gating tag.
func TestReleaseUnblockedChildren_WatchdogBlockedChildWithImplementationHistoryNotReleased(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	mkTracker(t, m, umb, 5)

	started := mkChild(t, m, "started", "Automaat/sybra#1", umb, nil, task.StatusBlocked)
	if err := m.AddRun(started.ID, task.AgentRun{
		AgentID: "a1",
		Role:    string(agent.RoleImplementation),
	}); err != nil {
		t.Fatalf("add implementation run: %v", err)
	}
	neverStarted := mkChild(t, m, "never-started", "Automaat/sybra#2", umb, nil, task.StatusBlocked)

	app.releaseUnblockedChildren(context.Background())

	startedTask := mustTask(t, m, started.ID)
	if startedTask.Status != task.StatusBlocked || !slices.Contains(startedTask.Tags, umbrellaGatedTag) {
		t.Fatalf("started child = %q tags=%v, want to stay blocked+gated (already implemented, not dependency-gated)",
			startedTask.Status, startedTask.Tags)
	}
	neverStartedTask := mustTask(t, m, neverStarted.ID)
	if neverStartedTask.Status != task.StatusTodo || slices.Contains(neverStartedTask.Tags, umbrellaGatedTag) {
		t.Fatalf("never-started child = %q tags=%v, want released to todo (dependency-gated with no history)",
			neverStartedTask.Status, neverStartedTask.Tags)
	}
}

func TestReleaseUnblockedChildren_RollupClosesUmbrella(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	var gotRepo string
	var gotNum, closes int
	app.umbrellaCloseIssue = func(repo string, number int, _ string) error {
		gotRepo, gotNum, closes = repo, number, closes+1
		return nil
	}
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	mkChild(t, m, "c1", "Automaat/sybra#1", umb, nil, task.StatusDone)
	mkChild(t, m, "c2", "Automaat/sybra#2", umb, nil, task.StatusDone)

	app.releaseUnblockedChildren(context.Background())

	if got := mustStatus(t, m, tracker.ID); got != task.StatusDone {
		t.Fatalf("tracker = %q, want done when all children done", got)
	}
	if closes != 1 || gotRepo != "Automaat/sybra" || gotNum != 100 {
		t.Fatalf("close = %d times repo=%q num=%d, want 1 Automaat/sybra 100", closes, gotRepo, gotNum)
	}
}

func TestReleaseUnblockedChildren_UpdatesTrackerProgressChecklist(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	if _, err := m.Update(tracker.ID, task.Update{Body: task.Ptr("Original body.")}); err != nil {
		t.Fatalf("seed tracker body: %v", err)
	}
	mkChild(t, m, "done child", "Automaat/sybra#1", umb, nil, task.StatusDone)
	mkChild(t, m, "blocked child", "Automaat/sybra#2", umb, nil, task.StatusHumanRequired)

	app.releaseUnblockedChildren(context.Background())

	body := mustTask(t, m, tracker.ID).Body
	for _, want := range []string{
		"Original body.",
		umbrellaProgressStart,
		"- [x] done child (#1) — done",
		"- [ ] blocked child (#2) — human-required",
		umbrellaProgressEnd,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("tracker body missing %q:\n%s", want, body)
		}
	}
}

func TestReleaseUnblockedChildren_ActiveExpansionPhaseHoldsReadyChild(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	if _, err := m.Update(tracker.ID, task.Update{
		Tags: task.Ptr([]string{"umbrella", umbrella.MaxParallelTag(5), umbrella.ExpandPhaseTag(umbrella.ExpandPhasePlanned)}),
	}); err != nil {
		t.Fatalf("mark tracker expanding: %v", err)
	}
	root := mkChild(t, m, "root", "Automaat/sybra#1", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	got := mustTask(t, m, root.ID)
	if got.Status != task.StatusTodo {
		t.Fatalf("root status = %q, want todo while expansion is still active", got.Status)
	}
	if !slices.Contains(got.Tags, umbrellaGatedTag) {
		t.Fatalf("root tags = %v, want child to stay gated until the full DAG is durable", got.Tags)
	}
}

func TestUmbrellaTrackerBody_ReplacesBlockAndIsIdempotent(t *testing.T) {
	t.Parallel()
	children := []umbrellaProgressChild{
		{title: "first", issue: "Automaat/sybra#1", status: task.StatusDone},
		{title: "second", issue: "Automaat/sybra#2", status: task.StatusInProgress},
	}
	input := "Intro\n\n" +
		umbrellaProgressStart + "\nold\n" + umbrellaProgressEnd +
		"\n\nTail"

	got := umbrellaTrackerBody(input, children)
	if !strings.Contains(got, "Intro\n\n"+umbrellaProgressStart) || !strings.Contains(got, umbrellaProgressEnd+"\n\nTail") {
		t.Fatalf("body outside generated block was not preserved:\n%s", got)
	}
	if strings.Contains(got, "old") {
		t.Fatalf("old generated block was not replaced:\n%s", got)
	}
	if again := umbrellaTrackerBody(got, children); again != got {
		t.Fatalf("umbrellaTrackerBody is not idempotent:\nfirst:\n%s\nsecond:\n%s", got, again)
	}
}

func TestUmbrellaTrackerBody_ReplacesMalformedBlockFromStart(t *testing.T) {
	t.Parallel()
	children := []umbrellaProgressChild{
		{title: "first", issue: "Automaat/sybra#1", status: task.StatusDone},
	}
	input := "Intro\n\n" +
		umbrellaProgressStart + "\nold without end\n\nTail that belonged to malformed block"

	got := umbrellaTrackerBody(input, children)
	if !strings.HasPrefix(got, "Intro\n\n"+umbrellaProgressStart) {
		t.Fatalf("body before malformed generated block was not preserved:\n%s", got)
	}
	for _, unwanted := range []string{"old without end", "Tail that belonged to malformed block"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("malformed generated block content %q was not replaced:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "- [x] first (#1) — done") || !strings.Contains(got, umbrellaProgressEnd) {
		t.Fatalf("replacement progress block missing expected content:\n%s", got)
	}
}

func TestRenderUmbrellaProgressBlock_EmptyChildren(t *testing.T) {
	t.Parallel()
	got := renderUmbrellaProgressBlock(nil)
	for _, want := range []string{
		umbrellaProgressStart,
		"## Subissues",
		"_No materialized subissues._",
		umbrellaProgressEnd,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("progress block missing %q:\n%s", want, got)
		}
	}
}

func TestTrackerRollup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		st              umbrellaState
		cyclic, settled bool
		want            task.Status
		wantClose       bool
	}{
		{"cycle", umbrellaState{total: 2}, true, true, task.StatusHumanRequired, false},
		{"stuck child", umbrellaState{total: 2, anyHR: true}, false, true, task.StatusHumanRequired, false},
		{"blocked child", umbrellaState{total: 2, anyBlocked: true}, false, true, task.StatusHumanRequired, false},
		{
			"cancelled child with no live sibling",
			umbrellaState{total: 2, children: []umbrellaProgressChild{
				{id: "a", issue: "Automaat/sybra#1", status: task.StatusCancelled},
				{id: "b", issue: "Automaat/sybra#2", status: task.StatusDone},
			}},
			false, true, task.StatusHumanRequired, false,
		},
		{
			// A cancelled task can never reach Done, so it must not count
			// against total — otherwise doneCount == total is impossible
			// forever, and the umbrella sits at in-progress with no signal.
			"cancelled duplicate with a live sibling on the same issue completes",
			umbrellaState{total: 2, doneCount: 1, children: []umbrellaProgressChild{
				{id: "a", issue: "Automaat/sybra#1", status: task.StatusCancelled},
				{id: "b", issue: "Automaat/sybra#1", status: task.StatusDone},
			}},
			false, true, task.StatusDone, true,
		},
		{
			// Excluding the resolved duplicate from total must not mask a
			// genuinely unfinished sibling under the same umbrella.
			"cancelled duplicate does not mask other still-running work",
			umbrellaState{total: 3, doneCount: 1, children: []umbrellaProgressChild{
				{id: "a", issue: "Automaat/sybra#1", status: task.StatusCancelled},
				{id: "b", issue: "Automaat/sybra#1", status: task.StatusDone},
				{id: "c", issue: "Automaat/sybra#2", status: task.StatusInProgress},
			}},
			false, true, task.StatusInProgress, false,
		},
		{"all done", umbrellaState{total: 2, doneCount: 2}, false, true, task.StatusDone, true},
		{"in progress", umbrellaState{total: 2, doneCount: 1}, false, true, task.StatusInProgress, false},
		{"zero children settled completes", umbrellaState{total: 0}, false, true, task.StatusDone, true},
		{"zero children not settled holds", umbrellaState{total: 0}, false, false, task.StatusInProgress, false},
		{
			"zero children settled with active expansion holds",
			umbrellaState{total: 0, tracker: &task.Task{
				Tags: []string{"umbrella", umbrella.ExpandPhaseTag(umbrella.ExpandPhaseMaterializing)},
			}},
			false, true, task.StatusInProgress, false,
		},
		{
			"zero children expand-failing below threshold never auto-closes",
			umbrellaState{total: 0, tracker: &task.Task{
				Status:       task.StatusInProgress,
				StatusReason: "umbrella expansion failed (attempt 1): boom",
				Tags:         []string{"umbrella", umbrella.ExpandFailTag(1)},
			}},
			false, true, task.StatusInProgress, false,
		},
		{
			"zero children expand-failing at threshold stays parked human-required",
			umbrellaState{total: 0, tracker: &task.Task{
				Status:       task.StatusHumanRequired,
				StatusReason: "umbrella expansion failed (attempt 3): boom",
				Tags:         []string{"umbrella", umbrella.ExpandFailTag(3)},
			}},
			false, true, task.StatusHumanRequired, false,
		},
		{
			"existing children expand-failing below threshold stays tracker-owned",
			umbrellaState{total: 2, doneCount: 1, tracker: &task.Task{
				Status:       task.StatusInProgress,
				StatusReason: "umbrella expansion failed (attempt 2): boom",
				Tags:         []string{"umbrella", umbrella.ExpandFailTag(2)},
			}},
			false, true, task.StatusInProgress, false,
		},
		{
			"all materialized children done does not close over fresh expand failure",
			umbrellaState{total: 2, doneCount: 2, tracker: &task.Task{
				Status:       task.StatusHumanRequired,
				StatusReason: "umbrella expansion failed (attempt 3): boom",
				Tags:         []string{"umbrella", umbrella.ExpandFailTag(3)},
			}},
			false, true, task.StatusHumanRequired, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := tt.st
			got, _, doClose := trackerRollup(&st, tt.cyclic, tt.settled)
			if got != tt.want || doClose != tt.wantClose {
				t.Fatalf("trackerRollup = (%q, close=%v), want (%q, close=%v)", got, doClose, tt.want, tt.wantClose)
			}
		})
	}
}

// TestReleaseUnblockedChildren_ExpandFailingTrackerNeverAutoCloses guards
// #1570: a tracker that exists only because internal/umbrella.Expand failed
// to materialize any children (planner killed at its timeout, repeatedly)
// must never be auto-closed as "no open sub-issues" — that previously
// closed the umbrella's GitHub issue while it still had open sub-issues that
// simply never got the chance to materialize as local tasks.
func TestReleaseUnblockedChildren_ExpandFailingTrackerNeverAutoCloses(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	closes := 0
	app.umbrellaCloseIssue = func(string, int, string) error { closes++; return nil }
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker, err := m.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:        task.Ptr(umb),
		TaskType:     task.Ptr(task.TaskTypeUmbrella),
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("umbrella expansion failed (attempt 3): plan umbrella: run planner: killed"),
		Tags:         task.Ptr([]string{"umbrella", umbrella.ExpandFailTag(3)}),
	})
	if err != nil {
		t.Fatalf("create failure tracker: %v", err)
	}

	app.releaseUnblockedChildren(context.Background())

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want to stay human-required (never auto-closed)", got)
	}
	if closes != 0 {
		t.Fatalf("umbrella issue closed %d times, want 0 while expansion keeps failing", closes)
	}
}

func TestReleaseUnblockedChildren_EmptyTrackerHeldUntilSettled(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	closes := 0
	app.umbrellaCloseIssue = func(string, int, string) error { closes++; return nil }
	const umb = "https://github.com/Automaat/sybra/issues/100"
	// Tracker with no children, created just now → not yet settled.
	tracker := mkTracker(t, m, umb, 5)

	app.releaseUnblockedChildren(context.Background())

	// A freshly-created childless tracker must not be closed — its children may
	// still be materializing in the same expansion.
	if got := mustStatus(t, m, tracker.ID); got != task.StatusInProgress {
		t.Fatalf("tracker = %q, want in-progress until settled", got)
	}
	if closes != 0 {
		t.Fatalf("childless tracker closed %d times before settling", closes)
	}
}

func TestReleaseUnblockedChildren_CancelledChildSurfaces(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	closes := 0
	app.umbrellaCloseIssue = func(string, int, string) error { closes++; return nil }
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	mkChild(t, m, "c1", "Automaat/sybra#1", umb, nil, task.StatusDone)
	mkChild(t, m, "c2", "Automaat/sybra#2", umb, nil, task.StatusCancelled)

	app.releaseUnblockedChildren(context.Background())

	// A cancelled child must surface for a human, never silently complete/close.
	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker = %q, want human-required on cancelled child", got)
	}
	if closes != 0 {
		t.Fatalf("umbrella was closed %d times despite a cancelled child", closes)
	}
}

func TestReleaseUnblockedChildren_PreservesBlockedTracker(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		childStatus task.Status
	}{
		{"human-required child", task.StatusHumanRequired},
		{"completed child", task.StatusDone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app, m := newUmbrellaGateApp(t)
			closes := 0
			app.umbrellaCloseIssue = func(string, int, string) error { closes++; return nil }
			const umb = "https://github.com/Automaat/sybra/issues/100"
			tracker := mkTracker(t, m, umb, 5)
			const reason = "auto-review: provider failure (local task abc12345; issue filing failed)"
			if _, err := m.Update(tracker.ID, task.Update{
				Status:       task.Ptr(task.StatusBlocked),
				StatusReason: task.Ptr(reason),
			}); err != nil {
				t.Fatalf("block tracker: %v", err)
			}
			mkChild(t, m, "child", "Automaat/sybra#1", umb, nil, tt.childStatus)

			app.releaseUnblockedChildren(context.Background())

			got := mustTask(t, m, tracker.ID)
			if got.Status != task.StatusBlocked {
				t.Fatalf("tracker = %q, want blocked", got.Status)
			}
			if got.StatusReason != reason {
				t.Fatalf("tracker reason = %q, want %q", got.StatusReason, reason)
			}
			if closes != 0 {
				t.Fatalf("umbrella was closed %d times while tracker blocked", closes)
			}
		})
	}
}

func TestReleaseUnblockedChildren_BlockedTrackerStillReleasesReadyChildren(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	const reason = "blocked awaiting operator action"
	if _, err := m.Update(tracker.ID, task.Update{
		Status:       task.Ptr(task.StatusBlocked),
		StatusReason: task.Ptr(reason),
	}); err != nil {
		t.Fatalf("block tracker: %v", err)
	}
	child := mkChild(t, m, "ready", "Automaat/sybra#1", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	gotTracker := mustTask(t, m, tracker.ID)
	if gotTracker.Status != task.StatusBlocked {
		t.Fatalf("tracker = %q, want blocked", gotTracker.Status)
	}
	if gotTracker.StatusReason != reason {
		t.Fatalf("tracker reason = %q, want %q", gotTracker.StatusReason, reason)
	}
	gotChild := mustTask(t, m, child.ID)
	if gotChild.Status != task.StatusTodo {
		t.Fatalf("child = %q, want todo", gotChild.Status)
	}
	if slices.Contains(gotChild.Tags, umbrellaGatedTag) {
		t.Fatalf("ready child still gated under blocked tracker: tags=%v", gotChild.Tags)
	}
	if gotChild.StatusReason != "umbrella dependencies satisfied" {
		t.Fatalf("child reason = %q, want release reason", gotChild.StatusReason)
	}
}

func TestReleaseUnblockedChildren_CloseFailureDefersDone(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	app.umbrellaCloseIssue = func(string, int, string) error { return errTestClose }
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	mkChild(t, m, "c1", "Automaat/sybra#1", umb, nil, task.StatusDone)

	app.releaseUnblockedChildren(context.Background())

	// Close failed transiently → tracker must NOT flip to done (so the close
	// retries next tick rather than orphaning the open issue).
	if got := mustStatus(t, m, tracker.ID); got != task.StatusInProgress {
		t.Fatalf("tracker = %q, want in-progress held for close retry", got)
	}
}

func newUmbrellaGateApp(t *testing.T) (*App, *task.Manager) {
	t.Helper()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	app := &App{tasks: tasks, logger: slog.New(slog.DiscardHandler), umbrellaRecoveryInFlight: make(map[string]bool)}
	return app, tasks
}

// mkChild creates a gate-managed umbrella child task in the given status,
// carrying the gating marker tag the expander would set.
func mkChild(t *testing.T, m *task.Manager, title, issue, umb string, deps []string, status task.Status) task.Task {
	t.Helper()
	tk, err := m.CreateFull(title, "", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr(issue),
		UmbrellaIssue: task.Ptr(umb),
		DependsOn:     task.Ptr(deps),
		Status:        task.Ptr(status),
		Tags:          task.Ptr([]string{umbrellaGatedTag}),
	})
	if err != nil {
		t.Fatalf("CreateFull(%s): %v", title, err)
	}
	return tk
}

func mustStatus(t *testing.T, m *task.Manager, id string) task.Status {
	t.Helper()
	return mustTask(t, m, id).Status
}

func mustTask(t *testing.T, m *task.Manager, id string) task.Task {
	t.Helper()
	tk, err := m.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return tk
}

func TestReleaseUnblockedChildren_ReleasesRootWithNoDeps(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	root := mkChild(t, m, "root", "Automaat/sybra#1", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	released, err := m.Get(root.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if released.Status != task.StatusTodo {
		t.Fatalf("root status = %q, want %q", released.Status, task.StatusTodo)
	}
	// The gating marker must be stripped on release so a later re-block cannot
	// retrigger a release.
	if slices.Contains(released.Tags, umbrellaGatedTag) {
		t.Fatalf("gating tag not stripped on release: tags=%v", released.Tags)
	}
}

// TestReleaseUnblockedChildren_HoldsOnCrossProgramBodyRef reproduces the real
// #2616 incident: a child's own DependsOn is fully satisfied (empty here),
// but its body names a free-text "strictly after #N" precondition on a
// different program's issue no Sybra task tracks. The gate must never
// release it, and must stamp a status reason naming the specific ref instead
// of leaving the generic reason a human/reviewer would have to re-derive
// every cycle.
func TestReleaseUnblockedChildren_HoldsOnCrossProgramBodyRef(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	mkTracker(t, m, umb, 5)

	child, err := m.CreateFull("child", "Ship this strictly after #2464 lands upstream.", task.AgentModeHeadless, task.Update{
		Issue:         task.Ptr("Automaat/sybra#1"),
		UmbrellaIssue: task.Ptr(umb),
		Status:        task.Ptr(task.StatusTodo),
		Tags:          task.Ptr([]string{umbrellaGatedTag}),
	})
	if err != nil {
		t.Fatalf("CreateFull: %v", err)
	}

	app.releaseUnblockedChildren(context.Background())

	held := mustTask(t, m, child.ID)
	if held.Status != task.StatusTodo || !slices.Contains(held.Tags, umbrellaGatedTag) {
		t.Fatalf("child released despite unmet cross-program ref: status=%q tags=%v", held.Status, held.Tags)
	}
	if !strings.Contains(held.StatusReason, "#2464") {
		t.Fatalf("status reason = %q, want it to name the unmet ref #2464", held.StatusReason)
	}
}

func TestReleaseUnblockedChildren_HoldsWhileUmbrellaExpanding(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	tracker := mkTracker(t, m, umb, 5)
	if _, err := m.Update(tracker.ID, task.Update{
		Tags: task.Ptr(append(slices.Clone(tracker.Tags), umbrella.ExpandingTag)),
	}); err != nil {
		t.Fatalf("mark tracker expanding: %v", err)
	}
	root := mkChild(t, m, "root", "Automaat/sybra#1", umb, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	held := mustTask(t, m, root.ID)
	if held.Status != task.StatusTodo || !slices.Contains(held.Tags, umbrellaGatedTag) {
		t.Fatalf("root released while umbrella is expanding: status=%q tags=%v", held.Status, held.Tags)
	}
	if got := mustStatus(t, m, tracker.ID); got != task.StatusInProgress {
		t.Fatalf("tracker status = %q, want unchanged while expanding", got)
	}
}

// TestReleaseUnblockedChildren_PushesReleaseToRemoteHomeNode reproduces the
// 2026-07-19 incident: a child already stamped AssignedNode by a prior
// Assigner.Tick (i.e. routed once, same as every real gated child) gets
// released locally by the umbrella gate, but without pushReleaseToHomeNode
// that release only ever lands in the leader's own canonical copy — the
// follower that actually dispatches the task never sees it.
func TestReleaseUnblockedChildren_PushesReleaseToRemoteHomeNode(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var assigned []task.Task
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/TaskService/AssignTask" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var args []task.Task
		_ = json.Unmarshal(body, &args)
		if len(args) == 1 {
			mu.Lock()
			assigned = append(assigned, args[0])
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "home-nas",
			Endpoints: []string{srv.URL},
			Homes:     []string{"Automaat/sybra"},
		}},
	}}
	roster, err := clusterlead.NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}

	app, m := newUmbrellaGateApp(t)
	app.cfg = cfg
	app.ctx = context.Background()
	app.assigner = clusterlead.NewAssigner(cfg, m, roster, func(string) bool { return false }, nil, nil)

	const umb = "https://github.com/Automaat/sybra/issues/100"
	child, _, err := m.Put(task.Task{
		ID:            "task-remote",
		Title:         "remote child",
		Status:        task.StatusTodo,
		ProjectID:     "Automaat/sybra",
		Issue:         "Automaat/sybra#1",
		UmbrellaIssue: umb,
		Tags:          []string{umbrellaGatedTag},
		AssignedNode:  "home-nas",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	app.releaseUnblockedChildren(context.Background())

	released, err := m.Get(child.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if slices.Contains(released.Tags, umbrellaGatedTag) {
		t.Fatalf("gating tag not stripped locally: tags=%v", released.Tags)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(assigned) != 1 {
		t.Fatalf("follower received %d pushes, want 1 — the release must reach the remote home node", len(assigned))
	}
	if assigned[0].Status != task.StatusTodo || slices.Contains(assigned[0].Tags, umbrellaGatedTag) {
		t.Errorf("pushed task = %+v, want released (todo, gate tag stripped)", assigned[0])
	}
}

// TestReleaseUnblockedChildren_RollsBackReleaseOnPushFailure covers the
// adversarial-review blocker on the fix above: if the remote push fails, the
// leader must not report the child as released while the follower — the node
// that actually dispatches it — never saw the change. Without the rollback,
// the gating tag/status only ever get stripped once, so a failed push
// permanently strands the follower with no way for the next tick to retry.
func TestReleaseUnblockedChildren_RollsBackReleaseOnPushFailure(t *testing.T) {
	t.Parallel()
	var failing atomic.Bool
	failing.Store(true)
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/TaskService/AssignTask" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempts.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "home-nas",
			Endpoints: []string{srv.URL},
			Homes:     []string{"Automaat/sybra"},
		}},
	}}
	roster, err := clusterlead.NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}

	app, m := newUmbrellaGateApp(t)
	app.cfg = cfg
	app.ctx = context.Background()
	app.assigner = clusterlead.NewAssigner(cfg, m, roster, func(string) bool { return false }, nil, nil)

	const umb = "https://github.com/Automaat/sybra/issues/100"
	child, _, err := m.Put(task.Task{
		ID:            "task-remote",
		Title:         "remote child",
		Status:        task.StatusBlocked,
		ProjectID:     "Automaat/sybra",
		Issue:         "Automaat/sybra#1",
		UmbrellaIssue: umb,
		Tags:          []string{umbrellaGatedTag},
		AssignedNode:  "home-nas",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	app.releaseUnblockedChildren(context.Background())

	if attempts.Load() != 1 {
		t.Fatalf("push attempts = %d, want 1", attempts.Load())
	}
	afterFailedPush, err := m.Get(child.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !slices.Contains(afterFailedPush.Tags, umbrellaGatedTag) {
		t.Fatalf("gating tag stripped despite failed push: tags=%v", afterFailedPush.Tags)
	}
	if afterFailedPush.Status != task.StatusBlocked {
		t.Fatalf("status = %q after failed push, want rolled back to %q", afterFailedPush.Status, task.StatusBlocked)
	}

	// Next tick, the follower is healthy again — the rolled-back state must
	// still be eligible for release rather than permanently stranded.
	failing.Store(false)
	app.releaseUnblockedChildren(context.Background())

	if attempts.Load() != 2 {
		t.Fatalf("push attempts after recovery = %d, want 2", attempts.Load())
	}
	afterRetry, err := m.Get(child.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if slices.Contains(afterRetry.Tags, umbrellaGatedTag) {
		t.Fatalf("gating tag still present after a successful retry: tags=%v", afterRetry.Tags)
	}
	if afterRetry.Status != task.StatusTodo {
		t.Fatalf("status = %q after successful retry, want %q", afterRetry.Status, task.StatusTodo)
	}
}

// TestReleaseUnblockedChildren_ConfidentialityDeclineDoesNotConsumeCap covers
// an adversarial-review finding: PushUpdate can decline to push (pushed=false)
// with no error at all — Assigner's confidentiality gate refuses to send a
// work-typed task to an untrusted/unencrypted follower and moves the task to
// its own Blocked+reason state instead. If releaseCapped read that nil-error
// as "released" (ignoring the pushed=false signal), it would count a task
// that never reached any follower against the umbrella's parallelism cap,
// starving a sibling that was genuinely ready. Cap=1 with a declined remote
// child and a releasable local child makes that starvation observable: the
// local child must still get the slot.
func TestReleaseUnblockedChildren_ConfidentialityDeclineDoesNotConsumeCap(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux) // plain HTTP: Encrypted() is false, same as an unconfigured follower
	t.Cleanup(srv.Close)

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "home-nas",
			Endpoints: []string{srv.URL},
			Homes:     []string{"Automaat/sybra"},
			// Trusted defaults to false.
		}},
	}}
	roster, err := clusterlead.NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}

	app, m := newUmbrellaGateApp(t)
	app.cfg = cfg
	app.ctx = context.Background()
	app.assigner = clusterlead.NewAssigner(cfg, m, roster, func(string) bool { return true }, nil, nil)

	const umb = "https://github.com/Automaat/sybra/issues/100"
	mkTracker(t, m, umb, 1) // cap=1: only one real release should fit this tick

	// IDs are ordered so os.ReadDir's lexical sort (Store.List's iteration
	// order, and thus ReadyToRelease's) puts the declined task first — the
	// scenario that actually exercises the cap-consumption bug. If the local
	// task were processed first it would legitimately take the cap=1 slot
	// before the declined task is even reached, masking the defect.
	declined, _, err := m.Put(task.Task{
		ID:            "task-a-declined",
		Title:         "remote child",
		Status:        task.StatusBlocked,
		ProjectID:     "Automaat/sybra", // homes to the untrusted follower above
		Issue:         "Automaat/sybra#1",
		UmbrellaIssue: umb,
		Tags:          []string{umbrellaGatedTag},
		AssignedNode:  "home-nas",
	})
	if err != nil {
		t.Fatalf("Put(declined): %v", err)
	}
	// Not in any follower's Homes list, so HomeNodeFor resolves it Local —
	// genuinely releasable this tick if the cap isn't wrongly consumed above.
	local, _, err := m.Put(task.Task{
		ID:            "task-b-local",
		Title:         "local child",
		Status:        task.StatusTodo,
		ProjectID:     "Automaat/other",
		Issue:         "Automaat/sybra#2",
		UmbrellaIssue: umb,
		Tags:          []string{umbrellaGatedTag},
	})
	if err != nil {
		t.Fatalf("Put(local): %v", err)
	}

	app.releaseUnblockedChildren(context.Background())

	if attempts.Load() != 0 {
		t.Fatalf("follower endpoint was hit %d times, want 0 — a work task must never reach an untrusted follower", attempts.Load())
	}
	gotDeclined, err := m.Get(declined.ID)
	if err != nil {
		t.Fatalf("Get(declined): %v", err)
	}
	if gotDeclined.Status != task.StatusBlocked {
		t.Fatalf("declined child status = %q, want %q (confidentiality-blocked, not released)", gotDeclined.Status, task.StatusBlocked)
	}
	if !strings.Contains(gotDeclined.StatusReason, "withheld") {
		t.Fatalf("declined child status reason = %q, want the confidentiality block's own reason", gotDeclined.StatusReason)
	}
	gotLocal, err := m.Get(local.ID)
	if err != nil {
		t.Fatalf("Get(local): %v", err)
	}
	if gotLocal.Status != task.StatusTodo || slices.Contains(gotLocal.Tags, umbrellaGatedTag) {
		t.Fatalf("local child = status=%q tags=%v, want released — the confidentiality decline must not have consumed cap=1's only slot", gotLocal.Status, gotLocal.Tags)
	}
}

// TestReleaseUnblockedChildren_SurvivesPostPushBookkeepingFailure covers a
// second adversarial-review finding: Assigner.route's local bookkeeping write
// after a successful AssignTask (re-stamping AssignedNode) can itself fail,
// in which case PushUpdate reports routed=true *and* a non-nil error. If
// releaseCapped read any non-nil error as "the follower never got it" and
// rolled back, the leader's local copy would revert to gated while the
// follower already holds the release — a split brain, and the exact silent
// divergence #2349 exists to close. The follower's AssignTask handler here
// deletes the task's on-disk file the instant it receives the push (after
// already recording receipt), deterministically forcing route()'s trailing
// a.tasks.Get to fail without relying on OS-specific permission semantics.
func TestReleaseUnblockedChildren_SurvivesPostPushBookkeepingFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	var logBuf bytes.Buffer
	app := &App{tasks: tasks, logger: slog.New(slog.NewTextHandler(&logBuf, nil)), umbrellaRecoveryInFlight: make(map[string]bool)}

	const childID = "task-remote"
	var received atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/TaskService/AssignTask" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		received.Add(1)
		// Simulate the push having already landed on the follower, then a
		// local-store fault on the leader before it finishes stamping
		// AssignedNode — the exact fault window route() documents handling.
		if err := os.Remove(filepath.Join(dir, childID+".md")); err != nil {
			t.Errorf("remove task file to force post-push bookkeeping failure: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "home-nas",
			Endpoints: []string{srv.URL},
			Homes:     []string{"Automaat/sybra"},
		}},
	}}
	roster, err := clusterlead.NewRoster(cfg, nil)
	if err != nil || roster == nil {
		t.Fatalf("NewRoster: roster=%v err=%v", roster, err)
	}
	app.cfg = cfg
	app.ctx = context.Background()
	app.assigner = clusterlead.NewAssigner(cfg, tasks, roster, func(string) bool { return false }, nil, nil)

	const umb = "https://github.com/Automaat/sybra/issues/100"
	if _, _, err := tasks.Put(task.Task{
		ID:            childID,
		Title:         "remote child",
		Status:        task.StatusBlocked,
		ProjectID:     "Automaat/sybra",
		Issue:         "Automaat/sybra#1",
		UmbrellaIssue: umb,
		Tags:          []string{umbrellaGatedTag},
		AssignedNode:  "home-nas",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	app.releaseUnblockedChildren(context.Background())

	if received.Load() != 1 {
		t.Fatalf("follower received %d pushes, want 1", received.Load())
	}
	// The handler deletes the task file the instant it receives the push, so
	// route()'s trailing a.tasks.Get is guaranteed to fail — confirming the
	// fault actually fired before asserting on how releaseCapped handled it.
	if _, err := os.Stat(filepath.Join(dir, childID+".md")); err == nil {
		t.Fatalf("task file unexpectedly still present — fault injection did not fire as intended")
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "umbrella.child.released") {
		t.Errorf("logs missing umbrella.child.released — a follower-acknowledged push must still count as a real release:\n%s", logs)
	}
	if !strings.Contains(logs, "umbrella.release.push_bookkeeping_failed") {
		t.Errorf("logs missing umbrella.release.push_bookkeeping_failed — the trailing local-write error must still be surfaced:\n%s", logs)
	}
	if strings.Contains(logs, "umbrella.release.push_failed") {
		t.Errorf("logs contain umbrella.release.push_failed — a push the follower already acknowledged must not be reported as a transport failure:\n%s", logs)
	}
	if strings.Contains(logs, "umbrella.release.rollback_failed") || strings.Contains(logs, "umbrella.release.rollback") {
		t.Errorf("logs show a rollback attempt — rolling back a release the follower already holds would split-brain leader and follower:\n%s", logs)
	}
}

func TestReleaseUnblockedChildren_IgnoresSybraBugBlock(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// A dependency that has merged.
	mkChild(t, m, "dep", "Automaat/sybra#1", umb, nil, task.StatusDone)
	// An umbrella child that ran, hit a Sybra bug, and was parked in `blocked`
	// by human-review WITHOUT the gating marker. Its dep is done, but the gate
	// must not resurrect it.
	bug, err := m.CreateFull("buggy", "", task.AgentModeHeadless, task.Update{
		Issue:          task.Ptr("Automaat/sybra#2"),
		UmbrellaIssue:  task.Ptr(umb),
		DependsOn:      task.Ptr([]string{"Automaat/sybra#1"}),
		Status:         task.Ptr(task.StatusBlocked),
		BlockedByIssue: task.Ptr("https://github.com/Automaat/sybra/issues/500"),
	})
	if err != nil {
		t.Fatalf("create buggy: %v", err)
	}

	app.releaseUnblockedChildren(context.Background())

	if got := mustStatus(t, m, bug.ID); got != task.StatusBlocked {
		t.Fatalf("sybra-bug-blocked child was released to %q, want blocked", got)
	}
}

func TestReleaseUnblockedChildren_HoldsThenReleasesChain(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// dep is still in progress; child depends on it.
	dep := mkChild(t, m, "dep", "Automaat/sybra#1", umb, nil, task.StatusInProgress)
	child := mkChild(t, m, "child", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())
	childTask := mustTask(t, m, child.ID)
	if childTask.Status != task.StatusTodo || !slices.Contains(childTask.Tags, umbrellaGatedTag) {
		t.Fatalf("child released early: status = %q tags = %v, want todo+%s", childTask.Status, childTask.Tags, umbrellaGatedTag)
	}

	// Finish the dependency, then the child must release.
	if _, err := m.Update(dep.ID, task.Update{Status: task.Ptr(task.StatusDone)}); err != nil {
		t.Fatalf("finish dep: %v", err)
	}
	app.releaseUnblockedChildren(context.Background())
	if got := mustStatus(t, m, child.ID); got != task.StatusTodo {
		t.Fatalf("child status = %q, want %q after dep done", got, task.StatusTodo)
	}
}

func TestReleaseUnblockedChildren_CrossFormDependencyResolves(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// Dependency recorded as a full URL; the child references it in shorthand.
	mkChild(t, m, "dep", "https://github.com/Automaat/sybra/issues/1", umb, nil, task.StatusDone)
	child := mkChild(t, m, "child", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())
	if got := mustStatus(t, m, child.ID); got != task.StatusTodo {
		t.Fatalf("child status = %q, want %q (cross-form dep should resolve)", got, task.StatusTodo)
	}
}

func TestReleaseUnblockedChildren_CycleFlagsTracker(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"

	// Tracker task for the umbrella.
	tracker, err := m.CreateFull("umbrella", "", task.AgentModeHeadless, task.Update{
		Issue:    task.Ptr(umb),
		TaskType: task.Ptr(task.TaskTypeUmbrella),
		Status:   task.Ptr(task.StatusInProgress),
	})
	if err != nil {
		t.Fatalf("create tracker: %v", err)
	}

	// x <-> y mutual dependency.
	x := mkChild(t, m, "x", "Automaat/sybra#1", umb, []string{"Automaat/sybra#2"}, task.StatusTodo)
	y := mkChild(t, m, "y", "Automaat/sybra#2", umb, []string{"Automaat/sybra#1"}, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	if got := mustStatus(t, m, tracker.ID); got != task.StatusHumanRequired {
		t.Fatalf("tracker status = %q, want %q on cycle", got, task.StatusHumanRequired)
	}
	// Cyclic children stay held (todo + gated tag, not released).
	if got := mustStatus(t, m, x.ID); got != task.StatusTodo {
		t.Fatalf("x status = %q, want todo (held gated)", got)
	}
	if got := mustStatus(t, m, y.ID); got != task.StatusTodo {
		t.Fatalf("y status = %q, want todo (held gated)", got)
	}
}

func TestReleaseUnblockedChildren_NoUmbrellaNoOp(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)

	// A plain blocked task with no umbrella must be left untouched.
	tk, err := m.CreateFull("plain", "", task.AgentModeHeadless, task.Update{
		Status:         task.Ptr(task.StatusBlocked),
		BlockedByIssue: task.Ptr("https://github.com/Automaat/sybra/issues/9"),
	})
	if err != nil {
		t.Fatalf("create plain: %v", err)
	}

	app.releaseUnblockedChildren(context.Background())
	if got := mustStatus(t, m, tk.ID); got != task.StatusBlocked {
		t.Fatalf("plain blocked task changed to %q, want blocked", got)
	}
}

// mustWriteProjectYAMLWithClone writes a minimal project YAML that resolves
// via project.Store.Get with ClonePath pointed at a real bare clone, so
// buildGroundLister can exercise the real DefaultBranch + ListTrackedFiles
// path without a network fetch.
func mustWriteProjectYAMLWithClone(t *testing.T, dir, id, clonePath string) {
	t.Helper()
	safe := strings.ReplaceAll(id, "/", "--")
	path := filepath.Join(dir, safe+".yaml")
	content := "id: " + id + "\ntype: pet\nowner: stub\nrepo: stub\nclone_path: " + clonePath + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write project YAML: %v", err)
	}
}

// newBareRepoWithTrackedFile creates a source repo with one committed file,
// bare-clones it, and returns the bare clone path and its default branch.
func newBareRepoWithTrackedFile(t *testing.T) (barePath, branch string) {
	t.Helper()
	src := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", src},
		{"git", "-C", src, "config", "user.email", "test@test.com"},
		{"git", "-C", src, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "internal", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "internal", "foo", "bar.go"), []byte("package foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", src, "add", "."},
		{"git", "-C", src, "commit", "-m", "init"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	branchOut, err := exec.Command("git", "-C", src, "branch", "--show-current").CombinedOutput()
	if err != nil {
		t.Fatalf("branch --show-current: %v: %s", err, branchOut)
	}
	branch = strings.TrimSpace(string(branchOut))

	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := project.CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	return bare, branch
}

func TestBuildGroundLister(t *testing.T) {
	t.Parallel()
	bare, _ := newBareRepoWithTrackedFile(t)

	dir := t.TempDir()
	store, err := project.NewStore(dir, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mustWriteProjectYAMLWithClone(t, dir, "o/r", bare)

	lister := buildGroundLister(store)

	t.Run("resolves tracked files for a registered project", func(t *testing.T) {
		t.Parallel()
		files, err := lister(context.Background(), "o/r")
		if err != nil {
			t.Fatalf("lister: %v", err)
		}
		if len(files) != 1 || files[0] != "internal/foo/bar.go" {
			t.Fatalf("files = %v, want [internal/foo/bar.go]", files)
		}
	})

	t.Run("fails open for an unregistered project", func(t *testing.T) {
		t.Parallel()
		if _, err := lister(context.Background(), "no/such-project"); err == nil {
			t.Fatal("expected an error for an unregistered project")
		}
	})
}

// TestReleaseUnblockedChildren_InFlightUmbrellaSkipsReleaseAndRollup guards
// the recovery/gate coordination contract: a ref marked recovering on App
// must not release ready children or roll up its tracker from a snapshot
// RecoverDegraded may be mutating concurrently.
func TestReleaseUnblockedChildren_InFlightUmbrellaSkipsReleaseAndRollup(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umb = "https://github.com/Automaat/sybra/issues/100"
	tracker := mkTracker(t, m, umb, 5)
	child := mkChild(t, m, "ready", "Automaat/sybra#1", umb, nil, task.StatusTodo)

	app.umbrellaRecoveryInFlight[umbrella.NormalizeIssueRef(umb)] = true

	app.releaseUnblockedChildren(context.Background())

	got := mustTask(t, m, child.ID)
	if got.Status != task.StatusTodo || !slices.Contains(got.Tags, umbrellaGatedTag) {
		t.Fatalf("child = status=%q tags=%v, want held gated while umbrella recovers", got.Status, got.Tags)
	}
	gotTracker := mustTask(t, m, tracker.ID)
	if gotTracker.Status != task.StatusInProgress {
		t.Fatalf("tracker status = %q, want unchanged while recovering", gotTracker.Status)
	}
}

// TestReleaseUnblockedChildren_InFlightUmbrellaDoesNotBlockUnrelated proves
// the in-flight exclusion is scoped to the recovering ref only — an
// unrelated umbrella's ready children still release in the same tick.
func TestReleaseUnblockedChildren_InFlightUmbrellaDoesNotBlockUnrelated(t *testing.T) {
	t.Parallel()
	app, m := newUmbrellaGateApp(t)
	const umbA = "https://github.com/Automaat/sybra/issues/100"
	const umbB = "https://github.com/Automaat/sybra/issues/200"
	mkTracker(t, m, umbA, 5)
	mkTracker(t, m, umbB, 5)
	childA := mkChild(t, m, "a", "Automaat/sybra#1", umbA, nil, task.StatusTodo)
	childB := mkChild(t, m, "b", "Automaat/sybra#2", umbB, nil, task.StatusTodo)

	app.umbrellaRecoveryInFlight[umbrella.NormalizeIssueRef(umbA)] = true

	app.releaseUnblockedChildren(context.Background())

	gotA := mustTask(t, m, childA.ID)
	if gotA.Status != task.StatusTodo || !slices.Contains(gotA.Tags, umbrellaGatedTag) {
		t.Fatalf("in-flight umbrella's child = %+v, want held", gotA)
	}
	gotB := mustTask(t, m, childB.ID)
	if gotB.Status != task.StatusTodo || slices.Contains(gotB.Tags, umbrellaGatedTag) {
		t.Fatalf("unrelated umbrella's child = %+v, want released", gotB)
	}
}

// TestReleaseUnblockedChildren_AsyncRecoveryDoesNotBlockUnrelatedRelease
// exercises the full releaseUnblockedChildren -> recoverDegradedUmbrellas
// scheduling path (not a manually pre-set in-flight marker): a degraded
// tracker's recovery is scheduled and left running (its recoverFn is stubbed
// to block), while an unrelated, non-degraded umbrella's ready child still
// releases in the very same gate tick.
func TestReleaseUnblockedChildren_AsyncRecoveryDoesNotBlockUnrelatedRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	fn, calls, release := countingRecoverFn()
	app := &App{
		tasks:                    tasks,
		logger:                   slog.New(slog.DiscardHandler),
		cfg:                      &config.Config{Umbrella: config.UmbrellaConfig{Enabled: true}},
		ctx:                      context.Background(),
		umbrellaRecoveryInFlight: make(map[string]bool),
		umbrellaRecoverFn:        fn,
	}
	defer release()

	const umbA = "https://github.com/Automaat/sybra/issues/300"
	const umbB = "https://github.com/Automaat/sybra/issues/301"
	mkDegradedTracker(t, tasks, umbA, task.StatusInProgress, "", "")
	mkTracker(t, tasks, umbB, 5)
	childB := mkChild(t, tasks, "b", "Automaat/sybra#1", umbB, nil, task.StatusTodo)

	app.releaseUnblockedChildren(context.Background())

	if !app.umbrellaRecoveryInFlightSnapshot()[umbrella.NormalizeIssueRef(umbA)] {
		t.Fatal("degraded umbrella ref not marked in-flight")
	}
	// recoverFn runs in its own goroutine (see recoverDegradedUmbrellas), so
	// its call only becomes visible once that goroutine is scheduled —
	// unlike the in-flight marker above, which is set synchronously before
	// the goroutine is spawned. waitFor lives in an e2e-tagged file, so poll
	// inline here rather than depend on it from an untagged test file.
	deadline := time.Now().Add(2 * time.Second)
	for len(calls()) != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := calls(); len(got) != 1 {
		t.Fatalf("recover calls = %v, want exactly 1 scheduled for the degraded umbrella", got)
	}

	gotB := mustTask(t, tasks, childB.ID)
	if gotB.Status != task.StatusTodo || slices.Contains(gotB.Tags, umbrellaGatedTag) {
		t.Fatalf("unrelated umbrella's child = %+v, want released despite concurrent recovery", gotB)
	}
}

// TestUmbrellaGroundingWired proves the wiring contract: the umbrella.Ground
// config toggle controls whether a lister-backed ExpandOption is threaded
// through, for both the GitHub-issues-fetcher path (app_init.go) and the
// manual/GUI expand path (services_wire_tasks.go). It exercises the same
// closure-building logic those files use rather than reaching into private
// App internals that would require a full app.Startup().
func TestUmbrellaGroundingWired(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := project.NewStore(dir, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	cases := []struct {
		name       string
		ground     bool
		wantLister bool
	}{
		{"ground enabled threads a grounder", true, true},
		{"ground disabled leaves grounding off", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var opts []umbrella.ExpandOption
			if tc.ground {
				opts = append(opts, umbrella.WithExpandGrounder(buildGroundLister(store), 0))
			}
			if got := len(opts) > 0; got != tc.wantLister {
				t.Fatalf("grounder threaded = %v, want %v", got, tc.wantLister)
			}
		})
	}
}
